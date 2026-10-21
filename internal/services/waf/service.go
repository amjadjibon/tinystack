// Package waf stores WAF Classic configuration records only, with no request filtering.
package waf

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName              = "waf"
	kindIPSet                = "IPSet"
	kindByteMatchSet         = "ByteMatchSet"
	kindSizeConstraintSet    = "SizeConstraintSet"
	kindSqlInjectionMatchSet = "SqlInjectionMatchSet"
	kindXssMatchSet          = "XssMatchSet"
	kindGeoMatchSet          = "GeoMatchSet"
	kindRegexPatternSet      = "RegexPatternSet"
	kindRegexMatchSet        = "RegexMatchSet"
	kindRule                 = "Rule"
	kindRateBasedRule        = "RateBasedRule"
	kindWebACL               = "WebACL"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
}

func New(store storage.Store) *Service { return &Service{store: store} }

func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

type record struct {
	Logging              *api.LoggingConfiguration
	Tags                 map[string]string
	IPSet                *api.IPSet
	ByteMatchSet         *api.ByteMatchSet
	SizeConstraintSet    *api.SizeConstraintSet
	SqlInjectionMatchSet *api.SqlInjectionMatchSet
	XssMatchSet          *api.XssMatchSet
	GeoMatchSet          *api.GeoMatchSet
	RegexPatternSet      *api.RegexPatternSet
	RegexMatchSet        *api.RegexMatchSet
	Rule                 *api.Rule
	RateBasedRule        *api.RateBasedRule
	WebACL               *api.WebACL
	ID, Name, ARN, Kind  string
}

type changeToken struct{ Status string }

func failure(code, message string) error { return awserror.New(code, message) }

func invalid(message string) error { return failure("WAFInvalidParameterException", message) }

func missing() error { return failure("WAFNonexistentItemException", "Resource does not exist") }

func key(scope request.Scope, kind, id string) string { return scope.Key(serviceName, kind, id) }

func read(tx storage.ReadTx, scope request.Scope, kind string, id *string) (*record, error) {
	if common.Value(id) == "" {
		return nil, invalid("Resource ID is required")
	}

	r, ok, err := storage.Read[record](tx, key(scope, kind, *id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, key(scope, r.Kind, r.ID), r)
}

// Tokens are consumed only when the surrounding resource transaction succeeds.
func consume(tx storage.WriteTx, scope request.Scope, token *string) error {
	if common.Value(token) == "" {
		return invalid("ChangeToken is required")
	}

	t, ok, err := storage.Read[changeToken](tx, key(scope, "token", *token))
	if err != nil {
		return err
	}

	if !ok || t.Status != "PROVISIONED" {
		return failure("WAFStaleDataException", "Change token is unknown or already used")
	}

	t.Status = "PENDING"
	return storage.Write(tx, key(scope, "token", *token), t)
}

func (s *Service) GetChangeToken(
	ctx context.Context,
	_ *api.GetChangeTokenRequest,
) (*api.GetChangeTokenResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	token := uuid.NewV4().String()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key(scope, "token", token), &changeToken{Status: "PROVISIONED"})
	})
	return &api.GetChangeTokenResponse{ChangeToken: new(token)}, err
}

func (s *Service) GetChangeTokenStatus(
	ctx context.Context,
	in *api.GetChangeTokenStatusRequest,
) (*api.GetChangeTokenStatusResponse, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var status string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if common.Value(in.ChangeToken) == "" {
			return invalid("ChangeToken is required")
		}

		t, ok, e := storage.Read[changeToken](tx, key(scope, "token", *in.ChangeToken))
		if e != nil {
			return e
		}

		if !ok {
			return missing()
		}

		status = t.Status
		if t.Status == "PENDING" {
			t.Status = "INSYNC"
			return storage.Write(tx, key(scope, "token", *in.ChangeToken), t)
		}

		return nil
	})
	return &api.GetChangeTokenStatusResponse{ChangeTokenStatus: new(status)}, err
}

func (s *Service) create(ctx context.Context, r *record, token *string) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	if len(r.Name) < 1 || len(r.Name) > 128 || strings.TrimSpace(r.Name) != r.Name {
		return invalid("Name requires 1-128 characters")
	}

	r.ID = uuid.NewV4().String()
	r.ARN = fmt.Sprintf("arn:aws:waf::%s:%s/%s", scope.AccountID, strings.ToLower(r.Kind), r.ID)
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := consume(tx, scope, token); e != nil {
			return e
		}

		rows, e := storage.Records[record](tx, scope.Prefix(serviceName, r.Kind))
		if e != nil {
			return e
		}

		for _, other := range rows {
			if other.Name == r.Name {
				return failure("WAFDuplicateItemException", "Name already exists")
			}
		}

		if e := validate(tx, scope, r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
}

func (s *Service) get(ctx context.Context, kind string, id *string) (*record, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error { var e error; r, e = read(tx, scope, kind, id); return e })
	return r, err
}

func (s *Service) change(ctx context.Context, kind string, id, token *string, fn func(*record) error) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := consume(tx, scope, token); e != nil {
			return e
		}

		r, e := read(tx, scope, kind, id)
		if e != nil {
			return e
		}

		if e := fn(r); e != nil {
			return e
		}

		if e := validate(tx, scope, r); e != nil {
			return e
		}

		return save(tx, scope, r)
	})
}

func (s *Service) remove(ctx context.Context, kind string, id, token *string) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := consume(tx, scope, token); e != nil {
			return e
		}

		r, e := read(tx, scope, kind, id)
		if e != nil {
			return e
		}

		if !empty(r) {
			return failure("WAFNonEmptyEntityException", "Remove all members before deleting")
		}

		var rows []*record
		// Each kind has its own prefix; tokens are deliberately excluded.
		for _, k := range resourceKinds {
			more, readErr := storage.Records[record](tx, scope.Prefix(serviceName, k))
			if readErr != nil {
				return readErr
			}

			rows = append(rows, more...)
		}

		for _, other := range rows {
			for _, ref := range references(other) {
				if ref.kind == kind && ref.id == r.ID {
					return failure("WAFReferencedItemException", "Resource is referenced")
				}
			}
		}

		tx.Delete(key(scope, kind, r.ID))
		return nil
	})
}

func (s *Service) list(ctx context.Context, kind string, marker *string, limit *int64) ([]*record, *string, error) {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	n := int64(100)
	if limit != nil {
		n = *limit
	}

	if n < 1 || n > 100 {
		return nil, nil, invalid("Limit must be 1-100")
	}

	var rows []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[record](tx, scope.Prefix(serviceName, kind))
		return e
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(rows, func(r *record) string { return r.ID }, marker, int(n), invalid("Invalid NextMarker"))
}

func apply[T any](items []T, action *string, item T) ([]T, error) {
	index := slices.IndexFunc(items, func(other T) bool { return reflect.DeepEqual(other, item) })
	switch common.Value(action) {
	case "INSERT":
		if index >= 0 {
			return nil, failure("WAFInvalidOperationException", "Member already exists")
		}

		return append(items, item), nil
	case "DELETE":
		if index < 0 {
			return nil, missing()
		}

		return slices.Delete(items, index, index+1), nil
	default:
		return nil, invalid("Action must be INSERT or DELETE")
	}
}
