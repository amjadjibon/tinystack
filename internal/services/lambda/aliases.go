package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"uuid"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// alias is a stored alias: a name for a version of a function, which may
// send a share of invocations to one other version.
type alias struct {
	// Weights maps the other version to the share of invocations it gets.
	Weights     map[string]float64 `json:",omitempty"`
	Name        string
	Function    string
	Version     string
	Description string
	RevisionID  string
}

func (a *alias) configuration(scope request.Scope) *api.AliasConfiguration {
	cfg := &api.AliasConfiguration{
		AliasArn:        new(qualifiedARN(scope, a.Function, a.Name)),
		Name:            new(a.Name),
		FunctionVersion: new(a.Version),
		Description:     new(a.Description),
		RevisionId:      new(a.RevisionID),
	}

	if len(a.Weights) > 0 {
		cfg.RoutingConfig = &api.AliasRoutingConfiguration{AdditionalVersionWeights: a.Weights}
	}

	return cfg
}

// route returns the version an invocation of the alias runs: another version
// for its share of invocations, otherwise the alias's own.
func (a *alias) route() string {
	draw := rand.Float64() //nolint:gosec // Traffic shifting needs no secure randomness.
	for version, weight := range a.Weights {
		if draw < weight {
			return version
		}
	}

	return a.Version
}

// setRouting validates and applies a routing configuration: at most one other
// published version with a weight from 0 to 1, and not with $LATEST. An empty
// configuration removes the routing.
func (a *alias) setRouting(tx storage.ReadTx, scope request.Scope, routing *api.AliasRoutingConfiguration) error {
	if routing != nil {
		a.Weights = nil
		if len(routing.AdditionalVersionWeights) > 0 {
			a.Weights = routing.AdditionalVersionWeights
		}
	}

	if len(a.Weights) > 1 {
		return invalid("RoutingConfig can shift traffic to only one additional version")
	}

	for version, weight := range a.Weights {
		if a.Version == latest || version == latest {
			return invalid("$LATEST is not supported for an alias pointing to more than 1 version")
		}

		if version == a.Version {
			return invalid("The additional version must differ from the alias's FunctionVersion")
		}

		if weight < 0 || weight > 1 {
			return invalid("Additional version weights must be between 0.0 and 1.0")
		}

		if err := checkTarget(tx, scope, a.Function, version); err != nil {
			return err
		}
	}

	return nil
}

func aliasKey(scope request.Scope, function, name string) string {
	return scopeKey(scope, kindAlias, function+"/"+name)
}

func loadAliasRecord(tx storage.ReadTx, key string) (*alias, bool, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, false, nil
	}

	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, false, fmt.Errorf("decode alias: %w", err)
	}

	return &a, true, nil
}

func loadAlias(tx storage.ReadTx, scope request.Scope, function, name string) (*alias, bool, error) {
	return loadAliasRecord(tx, aliasKey(scope, function, name))
}

// checkAliasName rejects a name that is not an alias name; a version number
// cannot be one.
func checkAliasName(name *string) error {
	if !aliasName.MatchString(common.Value(name)) || versionNumber.MatchString(common.Value(name)) {
		return invalid("Alias names must be 1 to 128 letters, digits, - or _, and not only digits")
	}

	return nil
}

// checkTarget rejects an alias target that is neither $LATEST nor a published
// version of the function.
func checkTarget(tx storage.ReadTx, scope request.Scope, function, version string) error {
	if version != latest && !versionNumber.MatchString(version) {
		return invalid("FunctionVersion must be $LATEST or a version number")
	}

	_, _, err := resolve(tx, scope, function, version)
	return err
}

func checkDescription(description *string) error {
	if len(common.Value(description)) > 256 {
		return invalid("Description must be at most 256 characters")
	}

	return nil
}

// aliasRequest resolves the unqualified function an alias operation names.
func aliasRequest(ctx context.Context, function, name *string) (m request.Metadata, fn string, err error) {
	if m, err = identity(ctx); err != nil {
		return m, "", err
	}

	fn, q, err := resolveName(m, common.Value(function))
	if err != nil {
		return m, "", err
	}

	if q != "" {
		return m, "", invalid("The function name of an alias operation cannot be qualified")
	}

	return m, fn, checkAliasName(name)
}

func (s *Service) CreateAlias(ctx context.Context, in *api.CreateAliasRequest) (*api.AliasConfiguration, error) {
	m, fn, err := aliasRequest(ctx, in.FunctionName, in.Name)
	if err != nil {
		return nil, err
	}

	if guardErr := checkDescription(in.Description); guardErr != nil {
		return nil, guardErr
	}

	a := &alias{
		Name:        *in.Name,
		Function:    fn,
		Version:     common.Value(in.FunctionVersion),
		Description: common.Value(in.Description),
		RevisionID:  uuid.NewV4().String(),
	}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, loadErr := resolve(tx, m.Scope, fn, ""); loadErr != nil {
			return loadErr
		}

		if _, exists, loadErr := loadAlias(tx, m.Scope, fn, a.Name); loadErr != nil || exists {
			if loadErr != nil {
				return loadErr
			}

			return conflict("Alias already exists: " + qualifiedARN(m.Scope, fn, a.Name))
		}

		if checkErr := checkTarget(tx, m.Scope, fn, a.Version); checkErr != nil {
			return checkErr
		}

		if routingErr := a.setRouting(tx, m.Scope, in.RoutingConfig); routingErr != nil {
			return routingErr
		}

		return writeJSON(tx, aliasKey(m.Scope, fn, a.Name), a)
	})
	if err != nil {
		return nil, err
	}

	return a.configuration(m.Scope), nil
}

// changeAlias applies change to an existing alias in one transaction.
func (s *Service) changeAlias(
	ctx context.Context,
	function, name *string,
	change func(storage.WriteTx, request.Scope, *alias) (bool, error),
) (*api.AliasConfiguration, error) {
	m, fn, err := aliasRequest(ctx, function, name)
	if err != nil {
		return nil, err
	}

	var a *alias
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var found bool
		var loadErr error
		if a, found, loadErr = loadAlias(tx, m.Scope, fn, *name); loadErr != nil {
			return loadErr
		}

		if !found {
			return notFound("Alias " + qualifiedARN(m.Scope, fn, *name))
		}

		keep, changeErr := change(tx, m.Scope, a)
		if changeErr != nil {
			return changeErr
		}

		if !keep {
			tx.Delete(aliasKey(m.Scope, fn, a.Name))
			return nil
		}

		return writeJSON(tx, aliasKey(m.Scope, fn, a.Name), a)
	})
	if err != nil {
		return nil, err
	}

	return a.configuration(m.Scope), nil
}

func (s *Service) GetAlias(ctx context.Context, in *api.GetAliasRequest) (*api.AliasConfiguration, error) {
	m, fn, err := aliasRequest(ctx, in.FunctionName, in.Name)
	if err != nil {
		return nil, err
	}

	var a *alias
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var found bool
		var loadErr error
		if a, found, loadErr = loadAlias(tx, m.Scope, fn, *in.Name); loadErr == nil && !found {
			return notFound("Alias " + qualifiedARN(m.Scope, fn, *in.Name))
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return a.configuration(m.Scope), nil
}

// UpdateAlias points an alias at another version or changes its description;
// with RevisionId, the alias must still be at that revision.
func (s *Service) UpdateAlias(ctx context.Context, in *api.UpdateAliasRequest) (*api.AliasConfiguration, error) {
	if err := checkDescription(in.Description); err != nil {
		return nil, err
	}

	return s.changeAlias(ctx, in.FunctionName, in.Name, func(tx storage.WriteTx, scope request.Scope, a *alias) (
		bool,
		error,
	) {
		if in.RevisionId != nil && *in.RevisionId != a.RevisionID {
			return false, revisionMismatch()
		}

		if in.FunctionVersion != nil {
			if err := checkTarget(tx, scope, a.Function, *in.FunctionVersion); err != nil {
				return false, err
			}

			a.Version = *in.FunctionVersion
		}

		if in.Description != nil {
			a.Description = *in.Description
		}

		if err := a.setRouting(tx, scope, in.RoutingConfig); err != nil {
			return false, err
		}

		a.RevisionID = uuid.NewV4().String()
		return true, nil
	})
}

func (s *Service) DeleteAlias(ctx context.Context, in *api.DeleteAliasRequest) (*api.DeleteAliasOutput, error) {
	_, err := s.changeAlias(ctx, in.FunctionName, in.Name,
		func(tx storage.WriteTx, scope request.Scope, a *alias) (bool, error) {
			tx.Delete(policyKey(scope, a.Function, a.Name))
			tx.Delete(provisionedKey(scope, a.Function, a.Name))
			tx.Delete(invokeConfigKey(scope, a.Function, a.Name))
			return false, deleteURLs(tx, scope, a.Function, a.Name)
		})
	if err != nil {
		return nil, err
	}

	return &api.DeleteAliasOutput{}, nil
}

// ListAliases lists a function's aliases in name order, or those of one
// version.
func (s *Service) ListAliases(ctx context.Context, in *api.ListAliasesRequest) (*api.ListAliasesResponse, error) {
	limit, err := pageSize(in.MaxItems, maxVersionItems)
	if err != nil {
		return nil, err
	}

	start, err := after(in.Marker)
	if err != nil {
		return nil, err
	}

	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	fn, q, err := resolveName(m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	if guardErr := unqualified(q); guardErr != nil {
		return nil, guardErr
	}

	var aliases []*alias
	prefix := scopeKey(m.Scope, kindAlias, fn+"/")
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if _, _, loadErr := resolve(tx, m.Scope, fn, ""); loadErr != nil {
			return loadErr
		}

		for _, k := range tx.Keys(prefix) {
			if strings.TrimPrefix(k, prefix) <= start {
				continue
			}

			a, _, loadErr := loadAliasRecord(tx, k)
			if loadErr != nil {
				return loadErr
			}

			if in.FunctionVersion == nil || a.Version == *in.FunctionVersion {
				aliases = append(aliases, a)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListAliasesResponse{Aliases: []api.AliasConfiguration{}}
	if int64(len(aliases)) > limit {
		aliases = aliases[:limit]
		out.NextMarker = nextMarker(aliases[limit-1].Name)
	}

	for _, a := range aliases {
		out.Aliases = append(out.Aliases, *a.configuration(m.Scope))
	}

	return out, nil
}
