// Package servicecatalog stores Service Catalog control-plane records. Templates and constraint
// parameters are never fetched, deployed, or enforced.
package servicecatalog

import (
	"context"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/idempotency"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "servicecatalog"
	kindPortfolio  = "portfolio"
	kindProduct    = "product"
	kindConstraint = "constraint"
)

// Service implements a records-only Service Catalog control plane.
type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type portfolio struct {
	Detail   api.PortfolioDetail
	Tags     map[string]string
	Products map[string]bool
}

type product struct {
	Detail api.ProductViewDetail
	Tags   map[string]string
}

type artifact struct {
	Detail api.ProvisioningArtifactDetail
	Info   map[string]string
}

type constraint struct {
	Detail     api.ConstraintDetail
	Parameters string
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidParametersException", fmt.Sprintf(format, args...))
}

func missing(kind, id string) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf("%s %s does not exist", kind, id))
}

func inUse() error { return awserror.New("ResourceInUseException", "Resource has associations") }

func read[T any](tx storage.ReadTx, scope request.Scope, kind, id string) (*T, error) {
	r, ok, err := storage.Read[T](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing(kind, id)
	}

	return r, nil
}

// transact persists successful token results atomically with their resource changes.
func transact[T any](ctx context.Context, s *Service, op string, token *string, input any,
	change func(storage.WriteTx, request.Scope) (*T, error),
) (*T, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	key := idempotency.Key(scope, serviceName, op, token)
	var out *T
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var found bool
		out, found, err = idempotency.Replay[*T](
			tx,
			key,
			input,
			invalid("IdempotencyToken was already used with different parameters"),
		)
		if err != nil || found {
			return err
		}

		out, err = change(tx, scope)
		if err != nil {
			return err
		}

		return idempotency.Remember(tx, key, input, out)
	})
	return out, err
}

func tagsMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		invalid("Tag Key and Value are required"), func(k string) error { return invalid("Duplicate tag key %s", k) })
	if err != nil {
		return nil, err
	}

	if err = tagging.CheckSet(out); err != nil {
		return nil, invalid("%s", err)
	}

	return out, nil
}

func tagsList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: new(k), Value: new(v)} })
}

func editTags(current map[string]string, add []api.Tag, remove []string) (map[string]string, error) {
	tags, err := tagsMap(add)
	if err != nil {
		return nil, err
	}

	out, err := tagging.Merge(tagging.Remove(common.CloneTags(current), remove), tags)
	if err != nil {
		return nil, invalid("%s", err)
	}

	return out, nil
}

func page[T any](items []T, id func(T) string, token *string, size *int64) (selected []T, next *string, err error) {
	limit := 20
	if size != nil && *size > 0 {
		limit = int(*size)
	}

	return common.Page(items, id, token, limit, invalid("Invalid PageToken"))
}

func resourceARN(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:catalog:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

var _ api.API = (*Service)(nil)

// All stored metadata is returned in English; translated representations are not implemented.
func englishLanguage(language *string) error {
	if language != nil && *language != "" && *language != "en" {
		return invalid("Only English (en) metadata is supported")
	}

	return nil
}
