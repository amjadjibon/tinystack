// Package amplify stores Amplify Hosting configuration without running builds or contacting providers.
// Jobs remain PENDING until stopped; domains remain PENDING_VERIFICATION. Upload and webhook URLs are inert.
package amplify

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName       = "amplify"
	kindApp           = "app"
	kindBranch        = "branch"
	jobRetry          = "RETRY"
	certificateCustom = "CUSTOM"
)

var _ api.API = (*Service)(nil)

// Service stores regional Amplify Hosting metadata.
type Service struct{ store storage.Store }

// New constructs a records-only Hosting service.
func New(store storage.Store) *Service { return &Service{store: store} }

// A record contains exactly one resource. Secrets are stored separately from response shapes.
type record struct {
	FileMap              map[string]string
	Backend              *api.BackendEnvironment
	Tags                 map[string]string
	BasicAuthCredentials *string
	JobReason            *string
	App                  *api.App
	Branch               *api.Branch
	Domain               *api.DomainAssociation
	Job                  *api.JobSummary
	Webhook              *api.Webhook
	ID                   string
	Kind                 string
	Name                 string
	ARN                  string
	AppID                string
	BranchName           string
	Started              bool
	Deployment           bool
}

func resourceKey(scope request.Scope, kind, id string) string {
	return scope.Key(serviceName, kind, id)
}

func childID(app, name string) string { return app + "/" + url.PathEscape(name) }
func resourceARN(scope request.Scope, path string) string {
	return fmt.Sprintf("arn:aws:amplify:%s:%s:%s", scope.Region, scope.AccountID, path)
}

func bad(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func missing() error { return awserror.New("NotFoundException", "Amplify resource does not exist.") }

func read(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, found, err := storage.Read[record](tx, resourceKey(scope, kind, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func parent(tx storage.ReadTx, scope request.Scope, app, branch string) error {
	if app == "" {
		return nil
	}

	if _, err := read(tx, scope, kindApp, app); err != nil {
		return err
	}

	if branch != "" {
		_, err := read(tx, scope, kindBranch, childID(app, branch))
		return err
	}

	return nil
}

// change serializes parent validation, read/modify/write, and cascading deletion in one transaction.
func (s *Service) change(ctx context.Context, kind, id, app, branch string, create, remove bool,
	apply func(*record, request.Scope) error,
) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := parent(tx, scope, app, branch); e != nil {
			return e
		}

		var found bool
		var e error
		r, found, e = storage.Read[record](tx, resourceKey(scope, kind, id))
		if e != nil {
			return e
		}

		if create {
			if found {
				return bad("Resource already exists.")
			}

			r = &record{ID: id, Kind: kind, AppID: app, BranchName: branch, Tags: map[string]string{}}
		} else if !found {
			return missing()
		}

		if e := apply(r, scope); e != nil {
			return e
		}

		if r.App != nil {
			r.Name = common.Value(r.App.Name)
		}

		if r.Webhook != nil {
			if e := parent(tx, scope, r.AppID, common.Value(r.Webhook.BranchName)); e != nil {
				return e
			}

			r.BranchName = common.Value(r.Webhook.BranchName)
		}

		if remove {
			tx.Delete(resourceKey(scope, kind, id))
			if kind == kindApp || kind == kindBranch {
				for _, k := range tx.Keys(strings.TrimSuffix(scope.Prefix(serviceName, ""), "/")) {
					child, _, e := storage.Read[record](tx, k)
					if e != nil {
						return e
					}

					if kind == kindApp && child.AppID == id ||
						kind == kindBranch && child.AppID == app && child.BranchName == r.Name {
						tx.Delete(k)
					}
				}
			}

			return nil
		}

		return storage.Write(tx, resourceKey(scope, kind, id), r)
	})
	return r, err
}

func (s *Service) get(ctx context.Context, kind, id string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, kind, id); return e })
	return r, err
}

func (s *Service) list(
	ctx context.Context,
	kind, app, branch, filter string,
	token *string,
	limit *int64,
	maxSize int,
) (
	[]*record, *string, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	size := maxSize
	if limit != nil && *limit > 0 {
		size = int(*limit)
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := parent(tx, scope, app, branch); e != nil {
			return e
		}

		all, e := storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		for _, r := range all {
			if app != "" && r.AppID != app || branch != "" && r.BranchName != branch ||
				filter != "" && r.Name != filter {
				continue
			}

			items = append(items, r)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, func(r *record) string { return r.ID }, token, size, bad("Invalid nextToken."))
}

func (r *record) appOutput() *api.App {
	out := common.Clone(r.App)
	out.Tags = common.CloneTags(r.Tags)
	if out.AutoBranchCreationConfig != nil {
		out.AutoBranchCreationConfig.BasicAuthCredentials = nil
	}

	return out
}

func (r *record) branchOutput() *api.Branch {
	out := common.Clone(r.Branch)
	out.Tags = common.CloneTags(r.Tags)
	return out
}

func (s *Service) tagged(ctx context.Context, arn *string, apply func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(common.Value(arn), resourceARN(scope, "apps/")) {
		return nil, missing()
	}

	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		for _, k := range tx.Keys(strings.TrimSuffix(scope.Prefix(serviceName, ""), "/")) {
			v, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if v.ARN != *arn || v.Kind != kindApp && v.Kind != kindBranch {
				continue
			}

			if e := apply(v); e != nil {
				return e
			}

			r = v
			return storage.Write(tx, k, v)
		}

		return missing()
	})
	return r, err
}
