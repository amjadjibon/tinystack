package lambda

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// maxVersionItems is the modeled MaxItems limit of ListVersionsByFunction and
// ListAliases.
const maxVersionItems = 10000

var (
	versionNumber = regexp.MustCompile(`^\d{1,10}$`)
	aliasName     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// versionID is the storage name of a published version of a function; the
// zero-padded number keeps versions in order.
func versionID(name, version string) string {
	n, _ := strconv.ParseInt(version, 10, 64)
	return fmt.Sprintf("%s/%010d", name, n)
}

// resolve loads the caller's function name at qualifier: $LATEST (also for an
// empty qualifier), a published version, or the version an alias points to.
// It also returns the storage key of the function's code.
func resolve(tx storage.ReadTx, scope request.Scope, name, qualifier string) (*function, string, error) {
	return resolveRouted(tx, scope, name, qualifier, false)
}

// resolveRouted is resolve that, with routed, follows an alias's routing
// configuration as an invocation does.
func resolveRouted(
	tx storage.ReadTx,
	scope request.Scope,
	name, qualifier string,
	routed bool,
) (*function, string, error) {
	record, code := scope.Key("lambda", kindFunction, name), scope.Key("lambda", kindCode, name)
	switch {
	case qualifier == "" || qualifier == latest:
	case versionNumber.MatchString(qualifier):
		record = scope.Key("lambda", kindVersion, versionID(name, qualifier))
		code = scope.Key("lambda", kindVersionCode, versionID(name, qualifier))
	default:
		a, found, err := loadAlias(tx, scope, name, qualifier)
		if err != nil {
			return nil, "", err
		}

		if !found {
			return nil, "", notFound("Function " + qualifiedARN(scope, name, qualifier))
		}

		if routed {
			return resolve(tx, scope, name, a.route())
		}

		return resolve(tx, scope, name, a.Version)
	}

	fn, found, err := loadFunction(tx, record)
	if err == nil && !found {
		err = notFound("Function " + qualifiedARN(scope, name, qualifier))
	}

	return fn, code, err
}

// unqualified rejects a qualifier on an operation that only takes $LATEST.
func unqualified(qualifier string) error {
	if qualifier != "" && qualifier != latest {
		return invalid("This operation does not support a qualified function name")
	}

	return nil
}

// publish publishes the $LATEST function fn, whose record it updates, as a
// new version unless the last version has the same code and configuration,
// which it returns instead.
func publish(
	tx storage.WriteTx,
	scope request.Scope,
	fn *function,
	description *string,
	now string,
) (*function, error) {
	if fn.Published > 0 {
		last, found, err := loadFunction(tx, scope.Key("lambda", kindVersion, versionID(fn.Name,
			strconv.FormatInt(fn.Published, 10))))
		if err != nil {
			return nil, err
		}

		if found && last.Source == fn.RevisionID {
			return last, nil
		}
	}

	v := *fn
	v.Tags, v.Published = nil, 0
	v.Version = strconv.FormatInt(fn.Published+1, 10)
	v.Source, v.RevisionID, v.LastModified = fn.RevisionID, uuid.NewV4().String(), now
	if description != nil {
		if v.Description = *description; len(v.Description) > 256 {
			return nil, invalid("Description must be at most 256 characters")
		}
	}

	id := versionID(fn.Name, v.Version)
	if code, ok := tx.Get(scope.Key("lambda", kindCode, fn.Name)); ok {
		tx.Put(scope.Key("lambda", kindVersionCode, id), code)
	}

	fn.Published++
	if err := writeJSON(tx, scope.Key("lambda", kindFunction, fn.Name), fn); err != nil {
		return nil, err
	}

	return &v, writeJSON(tx, scope.Key("lambda", kindVersion, id), &v)
}

// PublishVersion publishes $LATEST as a new version. With CodeSha256 or
// RevisionId, $LATEST must still have that code or revision.
func (s *Service) PublishVersion(
	ctx context.Context,
	in *api.PublishVersionRequest,
) (*api.FunctionConfiguration, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	if guardErr := unqualified(q); guardErr != nil {
		return nil, guardErr
	}

	var v *function
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		fn, _, loadErr := resolve(tx, m.Scope, name, "")
		if loadErr != nil {
			return loadErr
		}

		if in.RevisionId != nil && *in.RevisionId != fn.RevisionID {
			return revisionMismatch()
		}

		if in.CodeSha256 != nil && *in.CodeSha256 != fn.CodeSha256 {
			return invalid("CodeSHA256 (%s) is different from current CodeSHA256 in $LATEST (%s). "+
				"Please try again.", *in.CodeSha256, fn.CodeSha256)
		}

		var publishErr error
		v, publishErr = publish(tx, m.Scope, fn, in.Description, s.now().UTC().Format(timeFormat))
		return publishErr
	})
	if err != nil {
		return nil, err
	}

	cfg := v.configuration(m.Scope)
	return &cfg, nil
}

func revisionMismatch() error {
	return awserror.New("PreconditionFailedException", "The Revision Id provided does not match the latest "+
		"Revision Id. Call the GetFunction/GetAlias API to retrieve the latest Revision Id")
}

// pageSize validates MaxItems, which defaults to 50.
func pageSize(maxItems *int64, most int64) (int64, error) {
	if maxItems == nil {
		return listLimit, nil
	}

	if *maxItems < 1 || *maxItems > most {
		return 0, invalid("MaxItems must be between 1 and %d", most)
	}

	return *maxItems, nil
}

// ListVersionsByFunction lists $LATEST and then the published versions in
// order.
func (s *Service) ListVersionsByFunction(
	ctx context.Context,
	in *api.ListVersionsByFunctionRequest,
) (*api.ListVersionsByFunctionResponse, error) {
	limit, err := pageSize(in.MaxItems, maxVersionItems)
	if err != nil {
		return nil, err
	}

	start, err := after(in.Marker)
	if err != nil {
		return nil, err
	}

	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	if guardErr := unqualified(q); guardErr != nil {
		return nil, guardErr
	}

	// Versions page by storage name, with "" standing for $LATEST.
	var fns []*function
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		fn, _, loadErr := resolve(tx, m.Scope, name, "")
		if loadErr != nil {
			return loadErr
		}

		if in.Marker == nil {
			fns = append(fns, fn)
		}

		for _, k := range tx.Keys(m.Key("lambda", kindVersion, name+"/")) {
			if in.Marker != nil && strings.TrimPrefix(k, m.Key("lambda", kindVersion, "")) <= start {
				continue
			}

			v, _, versionErr := loadFunction(tx, k)
			if versionErr != nil {
				return versionErr
			}

			fns = append(fns, v)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	out := &api.ListVersionsByFunctionResponse{Versions: []api.FunctionConfiguration{}}
	if int64(len(fns)) > limit {
		fns = fns[:limit]
		last := fns[limit-1]
		out.NextMarker = nextMarker(versionID(name, "0"))
		if last.Version != "" {
			out.NextMarker = nextMarker(versionID(name, last.Version))
		}
	}

	for _, fn := range fns {
		out.Versions = append(out.Versions, fn.configuration(m.Scope))
	}

	return out, nil
}

// deleteVersion deletes a published version that no alias points to.
func (s *Service) deleteVersion(ctx context.Context, scope request.Scope, name, version string) error {
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, loadErr := resolve(tx, scope, name, version); loadErr != nil {
			return loadErr
		}

		var used []string
		for _, k := range tx.Keys(scope.Key("lambda", kindAlias, name+"/")) {
			a, _, loadErr := loadAliasRecord(tx, k)
			if loadErr != nil {
				return loadErr
			}

			if a.Version == version {
				used = append(used, a.Name)
			}
		}

		if len(used) > 0 {
			return conflict("Unable to delete version because the following aliases reference it: [" +
				strings.Join(used, ", ") + "]")
		}

		tx.Delete(scope.Key("lambda", kindVersion, versionID(name, version)))
		tx.Delete(scope.Key("lambda", kindVersionCode, versionID(name, version)))
		tx.Delete(policyKey(scope, name, version))
		tx.Delete(provisionedKey(scope, name, version))
		tx.Delete(invokeConfigKey(scope, name, version))
		return nil
	})
	if err != nil {
		return err
	}

	s.runtime.Release(qualifiedARN(scope, name, version))
	return nil
}

// deleteFunction deletes a function with its versions and aliases.
func (s *Service) deleteFunction(ctx context.Context, scope request.Scope, name string) error {
	arns := []string{functionARN(scope, name)}
	err := s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, _, loadErr := resolve(tx, scope, name, ""); loadErr != nil {
			return loadErr
		}

		tx.Delete(scope.Key("lambda", kindFunction, name))
		tx.Delete(scope.Key("lambda", kindCode, name))
		tx.Delete(reservedKey(scope, name))
		if urlErr := deleteURLs(tx, scope, name, ""); urlErr != nil {
			return urlErr
		}

		kinds := []string{kindVersion, kindVersionCode, kindAlias, kindPolicy, kindProvisioned, kindInvokeConfig}
		for _, kind := range kinds {
			for _, k := range tx.Keys(scope.Key("lambda", kind, name+"/")) {
				if kind == kindVersion {
					v, _, loadErr := loadFunction(tx, k)
					if loadErr != nil {
						return loadErr
					}

					arns = append(arns, v.arn(scope))
				}

				tx.Delete(k)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, arn := range arns {
		s.runtime.Release(arn)
	}

	return nil
}

// DeleteFunction deletes a function with its versions and aliases, or with a
// version qualifier only that version.
func (s *Service) DeleteFunction(
	ctx context.Context,
	in *api.DeleteFunctionRequest,
) (*api.DeleteFunctionResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, q, err := resolveName(&m, common.Value(in.FunctionName))
	if err != nil {
		return nil, err
	}

	if q, err = withQualifier(q, in.Qualifier); err != nil {
		return nil, err
	}

	switch {
	case q == "":
		err = s.deleteFunction(ctx, m.Scope, name)
	case q == latest:
		err = invalid("$LATEST version cannot be deleted without deleting the function.")
	case versionNumber.MatchString(q):
		err = s.deleteVersion(ctx, m.Scope, name, q)
	default:
		err = invalid("Deletion of aliases is not supported by DeleteFunction; use DeleteAlias.")
	}

	if err != nil {
		return nil, err
	}

	return &api.DeleteFunctionResponse{StatusCode: new(int64(http.StatusNoContent))}, nil
}
