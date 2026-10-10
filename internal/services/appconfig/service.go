// Package appconfig implements regional hosted configuration and immediate deployments.
package appconfig

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "appconfig"

const (
	kindApplication   = "application"
	kindEnvironment   = "environment"
	kindProfile       = "profile"
	kindStrategy      = "strategy"
	kindVersion       = "version"
	kindDeployment    = "deployment"
	growthLinear      = "LINEAR"
	growthExponential = "EXPONENTIAL"
	stateComplete     = "COMPLETE"
	stateReady        = "READY_FOR_DEPLOYMENT"
)

var _ api.API = (*Service)(nil)

// Records: <account>/<region>/appconfig/<kind>/<application>/<parent>/<id>.
// Tokens use the token kind, contain no content, and expire after 24 hours.
type record struct {
	ID, Name, Kind, ApplicationID, Parent string
	Tags                                  map[string]string
	Application                           *api.Application
	Environment                           *api.Environment
	Profile                               *api.ConfigurationProfile
	Strategy                              *api.DeploymentStrategy
	Hosted                                *hosted
	Deployment                            *api.Deployment
	Active                                map[string]string
	Previous                              string
	Counter                               int64
}

type hosted struct {
	ApplicationID, ProfileID, Description, ContentType, Label string
	Content                                                   []byte
	Number                                                    int64
}

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }
func bad(message string) error         { return awserror.New("BadRequestException", message) }
func missing() error                   { return awserror.New("ResourceNotFoundException", "Resource does not exist") }
func conflict(message string) error    { return awserror.New("ConflictException", message) }
func key(scope request.Scope, kind, app, parent, id string) string {
	return scope.Key(serviceName, kind, app+"/"+parent+"/"+id)
}

func resourceARN(scope request.Scope, r *record) string {
	base := fmt.Sprintf("arn:aws:appconfig:%s:%s:", scope.Region, scope.AccountID)
	switch r.Kind {
	case kindApplication:
		return base + "application/" + r.ID
	case kindStrategy:
		return base + "deploymentstrategy/" + r.ID
	case kindEnvironment:
		return base + "application/" + r.ApplicationID + "/environment/" + r.ID
	case kindProfile:
		return base + "application/" + r.ApplicationID + "/configurationprofile/" + r.ID
	case kindDeployment:
		return base + "application/" + r.ApplicationID + "/environment/" + r.Parent + "/deployment/" + strconv.FormatInt(
			common.Value(r.Deployment.DeploymentNumber),
			10,
		)
	}

	return base + "application/" + r.ApplicationID + "/configurationprofile/" + r.Parent +
		"/hostedconfigurationversion/" + strconv.FormatInt(r.Hosted.Number, 10)
}

func read(tx storage.ReadTx, scope request.Scope, kind, app, parent, id string) (*record, error) {
	if kind == kindStrategy {
		if r := predefined(id); r != nil {
			return r, nil
		}
	}

	r, found, err := storage.Read[record](tx, key(scope, kind, app, parent, id))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, key(scope, r.Kind, r.ApplicationID, r.Parent, r.ID), r)
}

func (s *Service) get(ctx context.Context, kind, app, parent, id string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, kind, app, parent, id); return e },
	)
	return r, err
}

func (s *Service) create(ctx context.Context, r *record) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	if strings.TrimSpace(r.Name) == "" {
		return bad("Name must not be empty")
	}

	if err = tagging.CheckSet(r.Tags); err != nil {
		return bad(err.Error())
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if r.ApplicationID != "" {
			if _, e := read(tx, scope, kindApplication, "", "", r.ApplicationID); e != nil {
				return e
			}
		}

		for _, k := range tx.Keys(scope.Key(serviceName, r.Kind, r.ApplicationID+"/"+r.Parent+"/")) {
			old, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if old.Name == r.Name {
				return conflict("Name already exists")
			}
		}

		return save(tx, scope, r)
	})
}

func (s *Service) edit(ctx context.Context, kind, app, id string, change func(*record) error) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, app, "", id)
		if e != nil {
			return e
		}

		if strings.HasPrefix(id, "AppConfig.") {
			return bad("AWS predefined strategies are immutable")
		}

		if changeErr := change(r); changeErr != nil {
			return changeErr
		}

		if r.Name == "" {
			return bad("Name must not be empty")
		}

		for _, k := range tx.Keys(scope.Key(serviceName, kind, app+"//")) {
			other, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if other.ID != id && other.Name == r.Name {
				return conflict("Name already exists")
			}
		}

		out = r
		return save(tx, scope, r)
	})
	return out, err
}

func (s *Service) remove(ctx context.Context, kind, app, id string) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := read(tx, scope, kind, app, "", id); e != nil {
			return e
		}

		if strings.HasPrefix(id, "AppConfig.") {
			return bad("AWS predefined strategies are immutable")
		}

		// Parents cannot disappear while child resources remain.
		for _, child := range []string{kindEnvironment, kindProfile, kindVersion, kindDeployment} {
			for _, k := range tx.Keys(scope.Prefix(serviceName, child)) {
				r, _, e := storage.Read[record](tx, k)
				if e != nil {
					return e
				}

				if (kind == kindApplication && r.ApplicationID == id) ||

					(kind == kindProfile && r.ApplicationID == app && r.Parent == id && child == kindVersion) {
					return conflict("Delete child resources first")
				}
			}
		}

		if kind == kindEnvironment {
			for _, k := range tx.Keys(scope.Key(serviceName, kindDeployment, app+"/"+id+"/")) {
				tx.Delete(k)
			}
		}

		tx.Delete(key(scope, kind, app, "", id))
		return nil
	})
}

func (s *Service) list(ctx context.Context, kind, app, parent string, token *string, maximum *int64,
	filter func(*record) bool,
) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit := int(common.Value(common.OrElse(maximum, new(int64(50)))))
	if limit < 1 || limit > 50 {
		return nil, nil, bad("MaxResults must be between 1 and 50")
	}

	var records []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if app != "" {
			if _, e := read(tx, scope, kindApplication, "", "", app); e != nil {
				return e
			}
		}

		if parent != "" {
			parentKind := kindProfile
			if kind == kindDeployment {
				parentKind = kindEnvironment
			}

			if _, e := read(tx, scope, parentKind, app, "", parent); e != nil {
				return e
			}
		}

		for _, k := range tx.Keys(scope.Key(serviceName, kind, app+"/"+parent+"/")) {
			r, _, e := storage.Read[record](tx, k)
			if e != nil {
				return e
			}

			if filter == nil || filter(r) {
				records = append(records, r)
			}
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	if kind == kindStrategy {
		records = append(records, predefinedStrategies()...)
		slices.SortFunc(records, func(a, b *record) int { return strings.Compare(a.ID, b.ID) })
	}

	return common.Page(records, func(r *record) string { return r.ID }, token, limit, bad("Invalid NextToken"))
}
