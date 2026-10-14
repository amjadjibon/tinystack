package cloudformation

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	// pageSize is how many items a page of a listing holds.
	pageSize = 100
	maxTags  = 50
)

var stackName = regexp.MustCompile(`^[a-zA-Z][-a-zA-Z0-9]{0,127}$`)

// updatable are the statuses a stack can be updated from.
var updatable = []string{statusCreateComplete, statusUpdateComplete, "UPDATE_ROLLBACK_COMPLETE", statusUpdateFailed}

func insufficientCapabilities(capability string) error {
	return awserror.New("InsufficientCapabilitiesException", "Requires capabilities : ["+capability+"]")
}

func alreadyExists(format string, args ...any) error {
	return awserror.New("AlreadyExistsException", fmt.Sprintf(format, args...))
}

func checkName(name string) error {
	if !stackName.MatchString(name) {
		return validation("1 validation error detected: Value '%s' at 'stackName' failed to satisfy constraint: "+
			"Member must satisfy regular expression pattern: [a-zA-Z][-a-zA-Z0-9]*", name)
	}

	return nil
}

func tagsOf(in []api.Tag) ([]tag, error) {
	if len(in) > maxTags {
		return nil, validation("1 validation error detected: Value at 'tags' failed to satisfy constraint: "+
			"Member must have length less than or equal to %d", maxTags)
	}

	out := make([]tag, 0, len(in))
	for _, t := range in {
		out = append(out, tag{Key: common.Value(t.Key), Value: common.Value(t.Value)})
	}

	return out, nil
}

// template returns the template a request gives in TemplateBody, or in the
// S3 object TemplateURL names.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) template(ctx context.Context, m request.Metadata, body, location *string) (string, error) {
	switch {
	case body != nil && location != nil:
		return "", validation("Specify exactly one of TemplateBody or TemplateUrl.")
	case body != nil:
		if len(*body) > maxBody {
			return "", validation("1 validation error detected: Value at 'templateBody' failed to satisfy constraint: "+
				"Member must have length less than or equal to %d", maxBody)
		}

		return *body, nil
	case location != nil:
		return s.fetch(ctx, m, *location)
	default:
		return "", validation("Either Template URL or Template Body must be specified.")
	}
}

// fetch reads a template from S3: virtual-hosted URLs name the bucket in the
// host, and any other URL, such as TinyStack's own endpoint, in the path.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) fetch(ctx context.Context, m request.Metadata, location string) (string, error) {
	invalid := validation("TemplateURL must be a supported URL.")
	u, err := url.Parse(location)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", invalid
	}

	host, path := u.Hostname(), strings.TrimPrefix(u.Path, "/")
	bucket, object, ok := strings.Cut(path, "/")
	if i := strings.Index(host, ".s3"); i > 0 && strings.HasSuffix(host, ".amazonaws.com") {
		bucket, object, ok = host[:i], path, path != ""
	}

	if !ok || bucket == "" || object == "" {
		return "", invalid
	}

	if s.caller == nil {
		return "", validation("TemplateURL needs TinyStack's S3")
	}

	c := &client{caller: s.caller, scope: m.Scope, endpoint: m.Endpoint}
	out, err := c.call(ctx, "s3", "GetObject", map[string]any{memberBucket: bucket, memberKey: object})
	if err != nil {
		return "", validation("TemplateURL must reference a valid S3 object to which you have access.")
	}

	body := outText(out, "Body")
	if len(body) > maxURLBody {
		return "", validation("Template may not exceed %d bytes in size.", maxURLBody)
	}

	return body, nil
}

// ssm returns a lookup of Systems Manager parameter values.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) ssm(ctx context.Context, m request.Metadata) func(string) (string, error) {
	return func(name string) (string, error) {
		if s.caller == nil {
			return "", validation("Parameters of SSM types need TinyStack's Systems Manager")
		}

		c := &client{caller: s.caller, scope: m.Scope, endpoint: m.Endpoint}
		out, err := c.call(ctx, "ssm", "GetParameter", map[string]any{memberName: name})
		if err != nil {
			return "", validation("Parameters: [ssm:%s] cannot be found.", name)
		}

		return outText(out, "Parameter", memberValue), nil
	}
}

// find reads a stack by name or ID; a deleted stack is found only by ID.
func find(tx storage.ReadTx, scope request.Scope, nameOrID string) (*stack, error) {
	name := nameOrID
	if strings.HasPrefix(nameOrID, "arn:") {
		parts := strings.Split(nameOrID, "/")
		if len(parts) != 3 {
			return nil, notExist(nameOrID)
		}

		name = parts[1]
	}

	st, found, err := storage.Read[stack](tx, scope.Key(service, kindStack, name))
	if err != nil {
		return nil, err
	}

	if found && (name == nameOrID || st.ID == nameOrID) {
		return st, nil
	}

	if name != nameOrID {
		old, found, err := storage.Read[stack](tx, scope.Key(service, kindDeleted, stackUUID(nameOrID)))
		if err != nil {
			return nil, err
		}

		if found && old.ID == nameOrID {
			return old, nil
		}
	}

	return nil, notExist(nameOrID)
}

// begin starts an operation on a stack and indexes it for the runner.
func (s *Service) begin(tx storage.WriteTx, scope request.Scope, st *stack, op *operation, status string) error {
	op.Started = s.now()
	st.Op, st.Token = op, op.Token
	if err := s.status(tx, scope, st, status, "User Initiated"); err != nil {
		return err
	}

	tx.Put(indexKey(scope, st.Name), []byte("{}"))
	return storage.Write(tx, scope.Key(service, kindStack, st.Name), st)
}

func onFailure(in *string, disableRollback *bool) (string, error) {
	switch {
	case in != nil && disableRollback != nil:
		return "", validation("You may specify either OnFailure or DisableRollback, but not both.")
	case common.Value(disableRollback):
		return onFailureNothing, nil
	case in == nil:
		return onFailureRollback, nil
	case *in == onFailureRollback || *in == "DELETE" || *in == onFailureNothing:
		return *in, nil
	default:
		return "", validation("Invalid OnFailure value %s", *in)
	}
}

func (s *Service) CreateStack(ctx context.Context, in *api.CreateStackInput) (*api.CreateStackOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name := common.Value(in.StackName)
	err = checkName(name)
	if err != nil {
		return nil, err
	}

	body, err := s.template(ctx, m, in.TemplateBody, in.TemplateURL)
	if err != nil {
		return nil, err
	}

	t, err := parse(body)
	if err != nil {
		return nil, err
	}

	err = t.requireCapabilities(in.Capabilities)
	if err != nil {
		return nil, err
	}

	err = t.requireExpand(in.Capabilities)
	if err != nil {
		return nil, err
	}

	failure, err := onFailure(in.OnFailure, in.DisableRollback)
	if err != nil {
		return nil, err
	}

	if common.Value(in.TimeoutInMinutes) < 0 {
		return nil, validation("TimeoutInMinutes must be greater than 0")
	}

	tags, err := tagsOf(in.Tags)
	if err != nil {
		return nil, err
	}

	params, err := resolveParameters(t, in.Parameters, nil, s.ssm(ctx, m))
	if err != nil {
		return nil, err
	}

	if checkErr := checkRules(t, m.Scope, name, params); checkErr != nil {
		return nil, checkErr
	}

	var id string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		existing, found, readErr := storage.Read[stack](tx, m.Key(service, kindStack, name))
		if readErr != nil {
			return readErr
		}

		if found {
			if in.ClientRequestToken != nil && existing.Token == *in.ClientRequestToken {
				id = existing.ID
				return nil
			}

			return alreadyExists("Stack [%s] already exists", name)
		}

		st := &stack{
			ID: stackARN(m.Scope, name), Name: name, Description: t.Description, Created: s.now(), Template: body,
			Parameters: params, Tags: tags, Capabilities: in.Capabilities, RoleARN: common.Value(in.RoleARN),
			Timeout: common.Value(in.TimeoutInMinutes), Protected: common.Value(in.EnableTerminationProtection),
			DisableRollback: failure == onFailureNothing, Endpoint: m.Endpoint, Resources: map[string]*resource{},
		}
		id = st.ID
		return s.begin(tx, m.Scope, st, &operation{
			Action: actionCreate, Phase: phaseApply, OnFailure: failure, Token: common.Value(in.ClientRequestToken),
		}, statusCreateInProgress)
	})
	if err != nil {
		return nil, err
	}

	s.poke()
	return &api.CreateStackOutput{StackId: &id}, nil
}

// current reads a stack for an operation outside a transaction.
func (s *Service) current(ctx context.Context, scope request.Scope, nameOrID string) (*stack, error) {
	var st *stack
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		st, err = find(tx, scope, nameOrID)
		return err
	})

	return st, err
}

// revise works out a stack's new template and parameters for an update or
// change set.
//
//nolint:gocritic // Metadata is an immutable request snapshot.
func (s *Service) revise(
	ctx context.Context, m request.Metadata, st *stack, body, location *string, previous *bool,
	given []api.Parameter, capabilities []string,
) (*template, string, []parameter, error) {
	text := st.Template
	if common.Value(previous) {
		if body != nil || location != nil {
			return nil, "", nil, validation(
				"You cannot specify both usePreviousTemplate and Template Body/Template URL",
			)
		}
	} else {
		var err error
		if text, err = s.template(ctx, m, body, location); err != nil {
			return nil, "", nil, err
		}
	}

	t, err := parse(text)
	if err != nil {
		return nil, "", nil, err
	}

	err = t.requireCapabilities(capabilities)
	if err != nil {
		return nil, "", nil, err
	}

	params, err := resolveParameters(t, given, st.Parameters, s.ssm(ctx, m))
	if err != nil {
		return nil, "", nil, err
	}

	if err = checkRules(t, m.Scope, st.Name, params); err != nil {
		return nil, "", nil, err
	}

	return t, text, params, nil
}

func (s *Service) UpdateStack(ctx context.Context, in *api.UpdateStackInput) (*api.UpdateStackOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	st, err := s.current(ctx, m.Scope, common.Value(in.StackName))
	if err != nil {
		return nil, err
	}

	if st.Deleted != nil {
		return nil, notExist(common.Value(in.StackName))
	}

	t, body, params, err := s.revise(ctx, m, st, in.TemplateBody, in.TemplateURL, in.UsePreviousTemplate,
		in.Parameters, in.Capabilities)
	if err != nil {
		return nil, err
	}

	err = t.requireExpand(in.Capabilities)
	if err != nil {
		return nil, err
	}

	tags := st.Tags
	if in.Tags != nil {
		if tags, err = tagsOf(in.Tags); err != nil {
			return nil, err
		}
	}

	failure := onFailureRollback
	if common.Value(in.DisableRollback) {
		failure = onFailureNothing
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		cur, findErr := find(tx, m.Scope, st.Name)
		if findErr != nil {
			return findErr
		}

		if in.ClientRequestToken != nil && cur.Token == *in.ClientRequestToken {
			return nil
		}

		if cur.Op != nil || !slices.Contains(updatable, cur.Status) {
			return validation("Stack:%s is in %s state and can not be updated.", cur.ID, cur.Status)
		}

		if body == cur.Template && slices.Equal(params, cur.Parameters) && slices.Equal(tags, cur.Tags) {
			return validation("No updates are to be performed.")
		}

		op := &operation{
			Action: actionUpdate, Phase: phaseApply, OnFailure: failure, Token: common.Value(in.ClientRequestToken),
			Previous: &revision{
				Template: cur.Template, Description: cur.Description, Parameters: cur.Parameters, Tags: cur.Tags,
			},
		}
		now := s.now()
		cur.Template, cur.Description, cur.Parameters, cur.Tags, cur.Updated = body, t.Description, params, tags, &now
		cur.Capabilities, cur.RoleARN, cur.Endpoint = in.Capabilities, common.Value(in.RoleARN), m.Endpoint
		return s.begin(tx, m.Scope, cur, op, statusUpdateInProgress)
	})
	if err != nil {
		return nil, err
	}

	s.poke()
	return &api.UpdateStackOutput{StackId: &st.ID}, nil
}

func (s *Service) DeleteStack(ctx context.Context, in *api.DeleteStackInput) (*api.DeleteStackOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if checkErr := checkDeploymentConfig(in.DeploymentConfig); checkErr != nil {
		return nil, checkErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, findErr := find(tx, m.Scope, common.Value(in.StackName))
		if findErr != nil {
			// Deleting a stack that does not exist succeeds.
			return nil //nolint:nilerr // DeleteStack is idempotent.
		}

		switch {
		case st.Deleted != nil:
			return nil
		case st.Op != nil && st.Op.Action == actionDelete:
			return nil
		case st.Op != nil:
			return validation("Stack [%s] cannot be deleted while in status %s", st.Name, st.Status)
		case st.Protected:
			return validation("Stack [%s] cannot be deleted while TerminationProtection is enabled", st.Name)
		case len(in.RetainResources) > 0 && st.Status != statusDeleteFailed:
			return validation("Invalid operation on stack [%s]. RetainResources can only be specified when "+
				"the stack is in the DELETE_FAILED state.", st.Name)
		}

		for _, logical := range in.RetainResources {
			if _, ok := st.Resources[logical]; !ok {
				return validation("The specified resources to retain must be in a valid state. Resource %s "+
					"does not exist in stack %s", logical, st.Name)
			}
		}

		for _, o := range st.Outputs {
			if o.Export == "" {
				continue
			}

			users, findErr := importers(tx, m.Scope, o.Export, st.Name)
			if findErr != nil {
				return findErr
			}

			if len(users) > 0 {
				return validation(
					"Export %s cannot be deleted as it is in use by %s",
					o.Export,
					strings.Join(users, ", "),
				)
			}
		}

		if st.Status == statusReview {
			return s.retire(tx, m.Scope, st)
		}

		return s.begin(tx, m.Scope, st, &operation{
			Action: actionDelete,
			Phase:  phaseRemove,
			Retain: in.RetainResources,
			Token:  common.Value(in.ClientRequestToken),
		}, statusDeleteInProgress)
	})
	if err != nil {
		return nil, err
	}

	s.poke()
	return &api.DeleteStackOutput{}, nil
}

// describe renders a stack as DescribeStacks does.
func describe(st *stack) api.Stack {
	out := api.Stack{
		StackId: &st.ID, StackName: &st.Name, CreationTime: new(st.Created), LastUpdatedTime: st.Updated,
		DeletionTime: st.Deleted, StackStatus: &st.Status, DisableRollback: new(st.DisableRollback),
		Capabilities: st.Capabilities, EnableTerminationProtection: new(st.Protected),
		DriftInformation: &api.StackDriftInformation{StackDriftStatus: new("NOT_CHECKED")},
	}
	if st.Description != "" {
		out.Description = &st.Description
	}

	if st.Reason != "" {
		out.StackStatusReason = &st.Reason
	}

	if st.RoleARN != "" {
		out.RoleARN = &st.RoleARN
	}

	if st.ChangeSetID != "" {
		out.ChangeSetId = &st.ChangeSetID
	}

	if st.Timeout > 0 {
		out.TimeoutInMinutes = &st.Timeout
	}

	out.Parameters = parametersOf(st.Parameters)
	for _, o := range st.Outputs {
		rendered := api.Output{OutputKey: new(o.Key), OutputValue: new(o.Value)}
		if o.Description != "" {
			rendered.Description = new(o.Description)
		}

		if o.Export != "" {
			rendered.ExportName = new(o.Export)
		}

		out.Outputs = append(out.Outputs, rendered)
	}

	out.Tags = apiTags(st.Tags)
	return out
}

// parametersOf renders parameters, hiding NoEcho values.
func parametersOf(params []parameter) []api.Parameter {
	var out []api.Parameter
	for _, p := range params {
		rendered := api.Parameter{ParameterKey: new(p.Key), ParameterValue: new(p.Value)}
		if p.NoEcho {
			rendered.ParameterValue = new("****")
		}

		if p.Resolved != "" {
			rendered.ResolvedValue = new(p.Resolved)
		}

		out = append(out, rendered)
	}

	return out
}

func apiTags(tags []tag) []api.Tag {
	var out []api.Tag
	for _, t := range tags {
		out = append(out, api.Tag{Key: new(t.Key), Value: new(t.Value)})
	}

	return out
}

// page returns the page of items a NextToken starts.
func page[T any](items []T, token *string) (out []T, next *string, err error) {
	start := 0
	if token != nil {
		n, err := strconv.Atoi(*token)
		if err != nil || n < 0 || n > len(items) {
			return nil, nil, validation("Invalid NextToken")
		}

		start = n
	}

	end := min(start+pageSize, len(items))
	if end < len(items) {
		next := strconv.Itoa(end)
		return items[start:end], &next, nil
	}

	return items[start:end], nil, nil
}

// stacks reads every stack in a scope that is not deleted.
func stacks(tx storage.ReadTx, scope request.Scope) ([]*stack, error) {
	var out []*stack
	for _, k := range tx.Keys(scope.Key(service, kindStack, "")) {
		st, _, err := storage.Read[stack](tx, k)
		if err != nil {
			return nil, err
		}

		out = append(out, st)
	}

	return out, nil
}

func (s *Service) DescribeStacks(ctx context.Context, in *api.DescribeStacksInput) (*api.DescribeStacksOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var found []*stack
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.StackName != nil {
			st, findErr := find(tx, m.Scope, *in.StackName)
			found = []*stack{st}
			return findErr
		}

		var loadErr error
		found, loadErr = stacks(tx, m.Scope)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(found, in.NextToken)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeStacksOutput{NextToken: next, Stacks: []api.Stack{}}
	for _, st := range items {
		out.Stacks = append(out.Stacks, describe(st))
	}

	return out, nil
}

func (s *Service) ListStacks(ctx context.Context, in *api.ListStacksInput) (*api.ListStacksOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var all []*stack
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		if all, loadErr = stacks(tx, m.Scope); loadErr != nil {
			return loadErr
		}

		for _, k := range tx.Keys(m.Key(service, kindDeleted, "")) {
			old, _, loadErr := storage.Read[stack](tx, k)
			if loadErr != nil {
				return loadErr
			}

			all = append(all, old)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	var summaries []api.StackSummary
	for _, st := range all {
		if len(in.StackStatusFilter) > 0 && !slices.Contains(in.StackStatusFilter, st.Status) {
			continue
		}

		summary := api.StackSummary{
			StackId: &st.ID, StackName: &st.Name, CreationTime: new(st.Created), LastUpdatedTime: st.Updated,
			DeletionTime: st.Deleted, StackStatus: &st.Status,
			DriftInformation: &api.StackDriftInformationSummary{StackDriftStatus: new("NOT_CHECKED")},
		}
		if st.Description != "" {
			summary.TemplateDescription = &st.Description
		}

		if st.Reason != "" {
			summary.StackStatusReason = &st.Reason
		}

		summaries = append(summaries, summary)
	}

	items, next, err := page(summaries, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.ListStacksOutput{StackSummaries: items, NextToken: next}, nil
}

func (s *Service) GetTemplate(ctx context.Context, in *api.GetTemplateInput) (*api.GetTemplateOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if stage := common.Value(in.TemplateStage); stage != "" && stage != "Original" && stage != stageProcessed {
		return nil, validation("Invalid TemplateStage %s", stage)
	}

	var body string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if in.ChangeSetName != nil {
			cs, findErr := findChangeSet(tx, m.Scope, common.Value(in.StackName), *in.ChangeSetName)
			if findErr != nil {
				return findErr
			}

			body = cs.Template
			return nil
		}

		if in.StackName == nil {
			return validation("StackName or ChangeSetName is required")
		}

		st, findErr := find(tx, m.Scope, *in.StackName)
		if findErr != nil {
			return findErr
		}

		body = st.Template
		return nil
	})
	if err != nil {
		return nil, err
	}

	if common.Value(in.TemplateStage) == stageProcessed {
		body = processed(body)
	}

	body = mqRedactTemplate(body)
	return &api.GetTemplateOutput{TemplateBody: &body, StagesAvailable: []string{"Original", stageProcessed}}, nil
}

func (s *Service) ValidateTemplate(
	ctx context.Context, in *api.ValidateTemplateInput,
) (*api.ValidateTemplateOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	body, err := s.template(ctx, m, in.TemplateBody, in.TemplateURL)
	if err != nil {
		return nil, err
	}

	t, err := parse(body)
	if err != nil {
		return nil, err
	}

	out := &api.ValidateTemplateOutput{
		Parameters: []api.TemplateParameter{}, DeclaredTransforms: append([]string{}, t.Transforms...),
	}
	if t.Description != "" {
		out.Description = &t.Description
	}

	for _, k := range slices.Sorted(maps.Keys(t.Parameters)) {
		p := t.Parameters[k]
		rendered := api.TemplateParameter{
			ParameterKey: new(k),
			DefaultValue: p.Default,
			NoEcho:       new(p.NoEcho),
		}
		if p.Description != "" {
			rendered.Description = new(p.Description)
		}

		out.Parameters = append(out.Parameters, rendered)
	}

	if caps, reason := t.capabilities(); len(caps) > 0 {
		out.Capabilities, out.CapabilitiesReason = caps, &reason
	}

	return out, nil
}

func (s *Service) GetTemplateSummary(
	ctx context.Context, in *api.GetTemplateSummaryInput,
) (*api.GetTemplateSummaryOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var body string
	if in.StackName != nil {
		if in.TemplateBody != nil || in.TemplateURL != nil {
			return nil, validation("Specify exactly one of StackName, TemplateBody, or TemplateURL.")
		}

		st, findErr := s.current(ctx, m.Scope, *in.StackName)
		if findErr != nil {
			return nil, findErr
		}

		body = st.Template
	} else if body, err = s.template(ctx, m, in.TemplateBody, in.TemplateURL); err != nil {
		return nil, err
	}

	t, err := parse(body)
	if err != nil {
		return nil, err
	}

	out := &api.GetTemplateSummaryOutput{
		Parameters:         []api.ParameterDeclaration{},
		ResourceTypes:      resourceTypes(t),
		DeclaredTransforms: append([]string{}, t.Transforms...),
	}
	if t.Description != "" {
		out.Description = &t.Description
	}

	if t.Version != "" {
		out.Version = &t.Version
	}

	if t.Metadata != nil {
		data, _ := json.Marshal(t.Metadata)
		out.Metadata = new(string(data))
	}

	for _, k := range slices.Sorted(maps.Keys(t.Parameters)) {
		p := t.Parameters[k]
		rendered := api.ParameterDeclaration{
			ParameterKey: new(
				k,
			),
			DefaultValue:  p.Default,
			ParameterType: new(p.Type),
			NoEcho:        new(p.NoEcho),
		}
		if p.Description != "" {
			rendered.Description = new(p.Description)
		}

		if len(p.AllowedValues) > 0 {
			rendered.ParameterConstraints = &api.ParameterConstraints{AllowedValues: p.AllowedValues}
		}

		out.Parameters = append(out.Parameters, rendered)
	}

	if caps, reason := t.capabilities(); len(caps) > 0 {
		out.Capabilities, out.CapabilitiesReason = caps, &reason
	}

	return out, nil
}

func (s *Service) DescribeStackEvents(
	ctx context.Context, in *api.DescribeStackEventsInput,
) (*api.DescribeStackEventsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var events []api.StackEvent
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		st, findErr := find(tx, m.Scope, common.Value(in.StackName))
		if findErr != nil {
			return findErr
		}

		keys := tx.Keys(m.Key(service, kindEvent, stackUUID(st.ID)+"/"))
		slices.Reverse(keys)
		for _, k := range keys {
			e, _, findErr := storage.Read[stackEvent](tx, k)
			if findErr != nil {
				return findErr
			}

			events = append(events, renderEvent(st, e))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(events, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.DescribeStackEventsOutput{StackEvents: items, NextToken: next}, nil
}

func renderEvent(st *stack, e *stackEvent) api.StackEvent {
	out := api.StackEvent{
		StackId: &st.ID, StackName: &st.Name, EventId: new(e.ID), LogicalResourceId: new(e.Logical),
		ResourceType: new(e.Type), Timestamp: new(e.Time), ResourceStatus: new(e.Status),
		PhysicalResourceId: new(e.Physical),
	}
	if e.Reason != "" {
		out.ResourceStatusReason = new(e.Reason)
	}

	if e.Properties != "" {
		out.ResourceProperties = new(e.Properties)
	}

	if e.Token != "" {
		out.ClientRequestToken = new(e.Token)
	}

	return out
}

// stackResource is one resource of a stack, for the resource operations.
type stackResource struct {
	st      *stack
	r       *resource
	logical string
}

// resourcesOf finds a stack's resources, or, with only a physical ID, the
// stack whose resource it is.
func resourcesOf(tx storage.ReadTx, scope request.Scope, name, logical, physical string) ([]stackResource, error) {
	var candidates []*stack
	switch {
	case name != "":
		st, err := find(tx, scope, name)
		if err != nil {
			return nil, err
		}

		candidates = []*stack{st}
	case physical != "":
		all, err := stacks(tx, scope)
		if err != nil {
			return nil, err
		}

		candidates = all
	default:
		return nil, validation("Either StackName or PhysicalResourceId must be specified.")
	}

	var out []stackResource
	for _, st := range candidates {
		for _, l := range slices.Sorted(maps.Keys(st.Resources)) {
			r := st.Resources[l]
			if (logical != "" && l != logical) || (physical != "" && r.Physical != physical) {
				continue
			}

			out = append(out, stackResource{st: st, r: r, logical: l})
		}
	}

	if physical != "" && name == "" && len(out) == 0 {
		return nil, validation("Stack for %s does not exist", physical)
	}

	return out, nil
}

func (s *Service) DescribeStackResource(
	ctx context.Context, in *api.DescribeStackResourceInput,
) (*api.DescribeStackResourceOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var found []stackResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		found, loadErr = resourcesOf(tx, m.Scope, common.Value(in.StackName), common.Value(in.LogicalResourceId), "")
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	if len(found) == 0 {
		return nil, validation("Resource %s does not exist for stack %s", common.Value(in.LogicalResourceId),
			common.Value(in.StackName))
	}

	sr := found[0]
	out := &api.StackResourceDetail{
		StackName:         &sr.st.Name,
		StackId:           &sr.st.ID,
		LogicalResourceId: new(sr.logical),
		PhysicalResourceId: new(
			sr.r.Physical,
		),
		ResourceType:         new(sr.r.Type),
		LastUpdatedTimestamp: new(sr.r.Updated),
		ResourceStatus:       new(sr.r.Status),
		Metadata:             new(sr.r.Metadata),
		DriftInformation:     &api.StackResourceDriftInformation{StackResourceDriftStatus: new("NOT_CHECKED")},
	}
	if sr.r.Reason != "" {
		out.ResourceStatusReason = new(sr.r.Reason)
	}

	return &api.DescribeStackResourceOutput{StackResourceDetail: out}, nil
}

func (s *Service) DescribeStackResources(
	ctx context.Context, in *api.DescribeStackResourcesInput,
) (*api.DescribeStackResourcesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var found []stackResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		found, loadErr = resourcesOf(tx, m.Scope, common.Value(in.StackName), common.Value(in.LogicalResourceId),
			common.Value(in.PhysicalResourceId))
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	out := &api.DescribeStackResourcesOutput{StackResources: []api.StackResource{}}
	for _, sr := range found {
		rendered := api.StackResource{
			StackName:         &sr.st.Name,
			StackId:           &sr.st.ID,
			LogicalResourceId: new(sr.logical),
			PhysicalResourceId: new(
				sr.r.Physical,
			),
			ResourceType:     new(sr.r.Type),
			Timestamp:        new(sr.r.Updated),
			ResourceStatus:   new(sr.r.Status),
			DriftInformation: &api.StackResourceDriftInformation{StackResourceDriftStatus: new("NOT_CHECKED")},
		}
		if sr.r.Reason != "" {
			rendered.ResourceStatusReason = new(sr.r.Reason)
		}

		out.StackResources = append(out.StackResources, rendered)
	}

	return out, nil
}

func (s *Service) ListStackResources(
	ctx context.Context, in *api.ListStackResourcesInput,
) (*api.ListStackResourcesOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var found []stackResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		found, loadErr = resourcesOf(tx, m.Scope, common.Value(in.StackName), "", "")
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	summaries := []api.StackResourceSummary{}
	for _, sr := range found {
		rendered := api.StackResourceSummary{
			LogicalResourceId: new(
				sr.logical,
			),
			PhysicalResourceId:   new(sr.r.Physical),
			ResourceType:         new(sr.r.Type),
			LastUpdatedTimestamp: new(sr.r.Updated),
			ResourceStatus:       new(sr.r.Status),
			DriftInformation: &api.StackResourceDriftInformationSummary{
				StackResourceDriftStatus: new("NOT_CHECKED"),
			},
		}
		if sr.r.Reason != "" {
			rendered.ResourceStatusReason = new(sr.r.Reason)
		}

		summaries = append(summaries, rendered)
	}

	items, next, err := page(summaries, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.ListStackResourcesOutput{StackResourceSummaries: items, NextToken: next}, nil
}

func (s *Service) ListExports(ctx context.Context, in *api.ListExportsInput) (*api.ListExportsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	exports := []api.Export{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		prefix := m.Key(service, kindExport, "")
		for _, k := range tx.Keys(prefix) {
			e, _, readErr := storage.Read[export](tx, k)
			if readErr != nil {
				return readErr
			}

			exports = append(exports, api.Export{
				ExportingStackId: new(
					e.StackID,
				),
				Name:  new(strings.TrimPrefix(k, prefix)),
				Value: new(e.Value),
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(exports, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.ListExportsOutput{Exports: items, NextToken: next}, nil
}

func (s *Service) ListImports(ctx context.Context, in *api.ListImportsInput) (*api.ListImportsOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var names []string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		names, loadErr = importers(tx, m.Scope, common.Value(in.ExportName), "")
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	if len(names) == 0 {
		return nil, validation("Export '%s' is not imported by any stack.", common.Value(in.ExportName))
	}

	items, next, err := page(names, in.NextToken)
	if err != nil {
		return nil, err
	}

	return &api.ListImportsOutput{Imports: items, NextToken: next}, nil
}

func (s *Service) UpdateTerminationProtection(
	ctx context.Context, in *api.UpdateTerminationProtectionInput,
) (*api.UpdateTerminationProtectionOutput, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var id string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		st, findErr := find(tx, m.Scope, common.Value(in.StackName))
		if findErr != nil {
			return findErr
		}

		if st.Deleted != nil {
			return notExist(common.Value(in.StackName))
		}

		st.Protected, id = common.Value(in.EnableTerminationProtection), st.ID
		return storage.Write(tx, m.Key(service, kindStack, st.Name), st)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateTerminationProtectionOutput{StackId: &id}, nil
}
