package sfn

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Versions are immutable copies of a state machine, numbered from 1 and never
// reused, under "…/sfn/version/<state machine>/<zero-padded number>", and
// aliases route executions to one or two versions by weight, under
// "…/sfn/alias/<state machine>/<alias>". A version's ARN is the state
// machine's with ":<number>" added, and an alias's with ":<alias>".

const (
	kindVersion = "version"
	kindAlias   = "alias"
	maxVersions = 1000
)

var aliasName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

// version is a published version of a state machine.
type version struct {
	Description string `json:",omitempty"`
	Machine     stateMachine
	Number      int64
}

// route is one version an alias sends a share of executions to.
type route struct {
	Version int64
	Weight  int64
}

// alias is a state machine alias's record.
type alias struct {
	Name        string
	Description string `json:",omitempty"`
	Routes      []route
	Created     float64
	Updated     float64
}

func versionKey(scope request.Scope, machine string, n int64) string {
	return fmt.Sprintf("%s%s/%010d", prefix(scope, kindVersion), machine, n)
}

func aliasKey(scope request.Scope, machine, name string) string {
	return prefix(scope, kindAlias) + machine + "/" + name
}

func machineARN(scope request.Scope, machine string) string {
	return arn(scope, arnStateMachine, machine)
}

// qualified splits a state machine ARN into its scope, state machine name,
// and qualifier: a version number, an alias name, or "".
func qualified(s string) (scope request.Scope, machine, qualifier string, err error) {
	scope, kind, name, ok := parseARN(s)
	machine, qualifier, _ = strings.Cut(name, ":")
	if !ok || kind != arnStateMachine || machine == "" || strings.Contains(qualifier, ":") {
		return scope, "", "", invalidARN(s)
	}

	return scope, machine, qualifier, nil
}

func isVersion(qualifier string) bool {
	n, err := strconv.ParseInt(qualifier, 10, 64)
	return err == nil && n > 0
}

func loadVersion(tx storage.ReadTx, scope request.Scope, machine string, n int64) (*version, error) {
	v, found, err := storage.Read[version](tx, versionKey(scope, machine, n))
	if err == nil && !found {
		err = awserror.New("ResourceNotFound", "State Machine Version Does Not Exist: '"+
			machineARN(scope, machine)+":"+strconv.FormatInt(n, 10)+"'")
	}

	return v, err
}

func loadAlias(tx storage.ReadTx, scope request.Scope, machine, name string) (*alias, error) {
	a, found, err := storage.Read[alias](tx, aliasKey(scope, machine, name))
	if err == nil && !found {
		err = awserror.New("ResourceNotFound", "Resource not found: '"+machineARN(scope, machine)+":"+name+"'")
	}

	return a, err
}

// target is what an execution runs: a state machine, or one of its versions,
// possibly reached through an alias.
type target struct {
	machine    *stateMachine
	scope      request.Scope
	versionARN string
	aliasARN   string
}

// loadTarget resolves a state machine, version, or alias ARN to what an
// execution of it runs. An alias picks one of its versions by weight.
func loadTarget(tx storage.ReadTx, arnString *string) (*target, error) {
	scope, machine, qualifier, err := qualified(common.Value(arnString))
	if err != nil {
		return nil, err
	}

	_, sm, err := loadMachine(tx, new(machineARN(scope, machine)))
	if err != nil {
		return nil, err
	}

	t := &target{machine: sm, scope: scope}
	if qualifier == "" {
		return t, nil
	}

	n, _ := strconv.ParseInt(qualifier, 10, 64)
	if !isVersion(qualifier) {
		a, aliasErr := loadAlias(tx, scope, machine, qualifier)
		if aliasErr != nil {
			return nil, aliasErr
		}

		t.aliasARN, n = *arnString, a.pick()
	}

	v, err := loadVersion(tx, scope, machine, n)
	if err != nil {
		return nil, err
	}

	t.machine, t.versionARN = &v.Machine, machineARN(scope, machine)+":"+strconv.FormatInt(n, 10)
	return t, nil
}

// pick chooses one of an alias's versions in proportion to its weight.
func (a *alias) pick() int64 {
	roll := rand.Int64N(100) // #nosec G404 -- routing, not security.
	for _, r := range a.Routes {
		if roll < r.Weight {
			return r.Version
		}

		roll -= r.Weight
	}

	return a.Routes[0].Version
}

// publish publishes a state machine's current revision as a new version, or
// returns the latest version when it already is that revision.
func publish(
	tx storage.WriteTx, scope request.Scope, sm *stateMachine, description string, now float64,
) (*version, error) {
	if sm.LastVersion > 0 {
		latest, found, err := storage.Read[version](tx, versionKey(scope, sm.Name, sm.LastVersion))
		if err != nil {
			return nil, err
		}

		if found && latest.Machine.RevisionID == sm.RevisionID {
			return latest, nil
		}
	}

	if len(tx.Keys(prefix(scope, kindVersion)+sm.Name+"/")) >= maxVersions {
		return nil, awserror.New("ServiceQuotaExceededException", "The state machine has 1000 versions.")
	}

	sm.LastVersion++
	v := &version{Description: description, Machine: *sm, Number: sm.LastVersion}
	v.Machine.Created = now
	if err := storage.Write(tx, versionKey(scope, sm.Name, v.Number), v); err != nil {
		return nil, err
	}

	return v, storage.Write(tx, machineKey(scope, sm.Name), sm)
}

func (v *version) arn(scope request.Scope) string {
	return machineARN(scope, v.Machine.Name) + ":" + strconv.FormatInt(v.Number, 10)
}

func (s *Service) PublishStateMachineVersion(
	ctx context.Context,
	in *api.PublishStateMachineVersionInput,
) (*api.PublishStateMachineVersionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if len(common.Value(in.Description)) > 256 {
		return nil, awserror.Validation("description must be at most 256 characters")
	}

	out := &api.PublishStateMachineVersionOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, sm, loadErr := loadMachine(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		if in.RevisionId != nil && *in.RevisionId != sm.RevisionID {
			return awserror.New("ConflictException", "The revisionId does not match the state machine's.")
		}

		v, publishErr := publish(tx, scope, sm, common.Value(in.Description), common.Unix(s.now()))
		if publishErr != nil {
			return publishErr
		}

		out.StateMachineVersionArn, out.CreationDate = new(v.arn(scope)), new(v.Machine.Created)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// ListStateMachineVersions lists a state machine's versions, newest first.
func (s *Service) ListStateMachineVersions(
	ctx context.Context,
	in *api.ListStateMachineVersionsInput,
) (*api.ListStateMachineVersionsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListStateMachineVersionsOutput{StateMachineVersions: []api.StateMachineVersionListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		scope, sm, loadErr := loadMachine(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		var items []api.StateMachineVersionListItem
		for _, k := range tx.Keys(prefix(scope, kindVersion) + sm.Name + "/") {
			v, _, readErr := storage.Read[version](tx, k)
			if readErr != nil {
				return readErr
			}

			items = append(items, api.StateMachineVersionListItem{
				StateMachineVersionArn: new(v.arn(scope)), CreationDate: new(v.Machine.Created),
			})
		}

		slices.Reverse(items)
		var pageErr error
		items, out.NextToken, pageErr = page(items, func(i api.StateMachineVersionListItem) string {
			return *i.StateMachineVersionArn
		}, in.NextToken, in.MaxResults)
		out.StateMachineVersions = append(out.StateMachineVersions, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeleteStateMachineVersion deletes a version that no alias routes to.
func (s *Service) DeleteStateMachineVersion(
	ctx context.Context,
	in *api.DeleteStateMachineVersionInput,
) (*api.DeleteStateMachineVersionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, machine, qualifier, err := qualified(common.Value(in.StateMachineVersionArn))
	if err == nil && !isVersion(qualifier) {
		err = invalidARN(common.Value(in.StateMachineVersionArn))
	}

	if err != nil {
		return nil, err
	}

	if scopeErr := inScope(&m, scope, in.StateMachineVersionArn); scopeErr != nil {
		return nil, scopeErr
	}

	n, _ := strconv.ParseInt(qualifier, 10, 64)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(prefix(scope, kindAlias) + machine + "/") {
			a, _, readErr := storage.Read[alias](tx, k)
			if readErr != nil {
				return readErr
			}

			if slices.ContainsFunc(a.Routes, func(r route) bool { return r.Version == n }) {
				return awserror.New("ConflictException", "Version "+qualifier+" is referenced by alias "+a.Name+
					"; update or delete the alias first.")
			}
		}

		tx.Delete(versionKey(scope, machine, n))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteStateMachineVersionOutput{}, nil
}

// checkRouting validates an alias's routing configuration, one or two
// versions of one state machine whose weights add up to 100, and returns
// the state machine and the routes.
func checkRouting(tx storage.ReadTx, m *request.Metadata, items []api.RoutingConfigurationListItem) (
	scope request.Scope, machine string, routes []route, err error,
) {
	if len(items) == 0 || len(items) > 2 {
		return scope, "", nil, awserror.Validation("routingConfiguration must have one or two versions")
	}

	total := int64(0)
	for _, item := range items {
		s, name, qualifier, qErr := qualified(common.Value(item.StateMachineVersionArn))
		switch {
		case qErr != nil || !isVersion(qualifier):
			return scope, "", nil, invalidARN(common.Value(item.StateMachineVersionArn))
		case machine != "" && (name != machine || s != scope):
			return scope, "", nil, awserror.Validation("routingConfiguration versions must belong to one " +
				"state machine")
		}

		if scopeErr := inScope(m, s, item.StateMachineVersionArn); scopeErr != nil {
			return scope, "", nil, scopeErr
		}

		scope, machine = s, name
		n, _ := strconv.ParseInt(qualifier, 10, 64)
		if _, loadErr := loadVersion(tx, scope, machine, n); loadErr != nil {
			return scope, "", nil, loadErr
		}

		weight := common.Value(item.Weight)
		if weight < 0 || weight > 100 || slices.ContainsFunc(routes, func(r route) bool { return r.Version == n }) {
			return scope, "", nil, awserror.Validation("routingConfiguration needs distinct versions with " +
				"weights from 0 to 100")
		}

		total += weight
		routes = append(routes, route{Version: n, Weight: weight})
	}

	if total != 100 {
		return scope, "", nil, awserror.Validation("routingConfiguration weights must add up to 100")
	}

	return scope, machine, routes, nil
}

func (s *Service) CreateStateMachineAlias(
	ctx context.Context,
	in *api.CreateStateMachineAliasInput,
) (*api.CreateStateMachineAliasOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.Name)
	switch {
	case !aliasName.MatchString(name) || isVersion(name):
		return nil, awserror.New("InvalidName", "Invalid Name: '"+name+"'")
	case len(common.Value(in.Description)) > 256:
		return nil, awserror.Validation("description must be at most 256 characters")
	}

	out := &api.CreateStateMachineAliasOutput{}
	now := common.Unix(s.now())
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		scope, machine, routes, routeErr := checkRouting(tx, &m, in.RoutingConfiguration)
		if routeErr != nil {
			return routeErr
		}

		out.StateMachineAliasArn = new(machineARN(scope, machine) + ":" + name)
		existing, found, readErr := storage.Read[alias](tx, aliasKey(scope, machine, name))
		switch {
		case readErr != nil:
			return readErr
		case found && existing.Description == common.Value(in.Description) && slices.Equal(existing.Routes, routes):
			out.CreationDate = new(existing.Created)
			return nil
		case found:
			return awserror.New("ConflictException", "An alias with this name already exists: '"+
				*out.StateMachineAliasArn+"'")
		}

		a := &alias{Name: name, Description: common.Value(in.Description), Routes: routes, Created: now, Updated: now}
		out.CreationDate = new(now)
		return storage.Write(tx, aliasKey(scope, machine, name), a)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// aliasRef resolves an alias ARN in the caller's scope.
func aliasRef(m *request.Metadata, arnString *string) (scope request.Scope, machine, name string, err error) {
	scope, machine, name, err = qualified(common.Value(arnString))
	if err == nil && (name == "" || isVersion(name)) {
		err = invalidARN(common.Value(arnString))
	}

	if err == nil {
		err = inScope(m, scope, arnString)
	}

	return scope, machine, name, err
}

func (a *alias) routing(scope request.Scope, machine string) []api.RoutingConfigurationListItem {
	out := make([]api.RoutingConfigurationListItem, len(a.Routes))
	for i, r := range a.Routes {
		out[i] = api.RoutingConfigurationListItem{
			StateMachineVersionArn: new(machineARN(scope, machine) + ":" + strconv.FormatInt(r.Version, 10)),
			Weight:                 new(r.Weight),
		}
	}

	return out
}

func (s *Service) DescribeStateMachineAlias(
	ctx context.Context,
	in *api.DescribeStateMachineAliasInput,
) (*api.DescribeStateMachineAliasOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, machine, name, err := aliasRef(&m, in.StateMachineAliasArn)
	if err != nil {
		return nil, err
	}

	var out *api.DescribeStateMachineAliasOutput
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		a, loadErr := loadAlias(tx, scope, machine, name)
		if loadErr != nil {
			return loadErr
		}

		out = &api.DescribeStateMachineAliasOutput{
			StateMachineAliasArn: in.StateMachineAliasArn,
			Name:                 new(a.Name),
			RoutingConfiguration: a.routing(scope, machine),
			CreationDate:         new(a.Created),
			UpdateDate:           new(a.Updated),
		}
		if a.Description != "" {
			out.Description = new(a.Description)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateStateMachineAlias(
	ctx context.Context,
	in *api.UpdateStateMachineAliasInput,
) (*api.UpdateStateMachineAliasOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, machine, name, err := aliasRef(&m, in.StateMachineAliasArn)
	switch {
	case err != nil:
		return nil, err
	case in.Description == nil && in.RoutingConfiguration == nil:
		return nil, awserror.Validation("Either the description or the routingConfiguration must be specified")
	case len(common.Value(in.Description)) > 256:
		return nil, awserror.Validation("description must be at most 256 characters")
	}

	now := common.Unix(s.now())
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		a, loadErr := loadAlias(tx, scope, machine, name)
		if loadErr != nil {
			return loadErr
		}

		if in.RoutingConfiguration != nil {
			routeScope, routeMachine, routes, routeErr := checkRouting(tx, &m, in.RoutingConfiguration)
			if routeErr != nil {
				return routeErr
			}

			if routeScope != scope || routeMachine != machine {
				return awserror.Validation("routingConfiguration versions must belong to the alias's state machine")
			}

			a.Routes = routes
		}

		if in.Description != nil {
			a.Description = *in.Description
		}

		a.Updated = now
		return storage.Write(tx, aliasKey(scope, machine, name), a)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateStateMachineAliasOutput{UpdateDate: &now}, nil
}

func (s *Service) DeleteStateMachineAlias(
	ctx context.Context,
	in *api.DeleteStateMachineAliasInput,
) (*api.DeleteStateMachineAliasOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope, machine, name, err := aliasRef(&m, in.StateMachineAliasArn)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, loadErr := loadAlias(tx, scope, machine, name); loadErr != nil {
			return loadErr
		}

		tx.Delete(aliasKey(scope, machine, name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteStateMachineAliasOutput{}, nil
}

// ListStateMachineAliases lists a state machine's aliases in name order.
func (s *Service) ListStateMachineAliases(
	ctx context.Context,
	in *api.ListStateMachineAliasesInput,
) (*api.ListStateMachineAliasesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.ListStateMachineAliasesOutput{StateMachineAliases: []api.StateMachineAliasListItem{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		scope, sm, loadErr := loadMachine(tx, in.StateMachineArn)
		if loadErr != nil {
			return loadErr
		}

		if scopeErr := inScope(&m, scope, in.StateMachineArn); scopeErr != nil {
			return scopeErr
		}

		var items []api.StateMachineAliasListItem
		for _, k := range tx.Keys(prefix(scope, kindAlias) + sm.Name + "/") {
			a, _, readErr := storage.Read[alias](tx, k)
			if readErr != nil {
				return readErr
			}

			items = append(items, api.StateMachineAliasListItem{
				StateMachineAliasArn: new(machineARN(scope, sm.Name) + ":" + a.Name), CreationDate: new(a.Created),
			})
		}

		var pageErr error
		items, out.NextToken, pageErr = page(items, func(i api.StateMachineAliasListItem) string {
			return *i.StateMachineAliasArn
		}, in.NextToken, in.MaxResults)
		out.StateMachineAliases = append(out.StateMachineAliases, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
