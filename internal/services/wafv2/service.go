// Package wafv2 persists WAF configuration records. It does not inspect traffic.
package wafv2

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "wafv2"
	regional    = "REGIONAL"
	cloudfront  = "CLOUDFRONT"
	kindACL     = "webacl"
	kindIP      = "ipset"
	kindGroup   = "rulegroup"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

// Configuration, identity, tags and lock token share one transaction boundary.
// Scope is part of the storage key, including in us-east-1 where both scopes exist.
type record struct {
	Tags                                      map[string]string
	Description                               *string
	DefaultAction                             *api.DefaultAction
	Visibility                                *api.VisibilityConfig
	Name, ID, ARN, Kind, Scope, Token, Family string
	Rules                                     []api.Rule
	Addresses                                 []string
	Capacity                                  int64
	Created                                   float64
}

func failure(code, message string) error { return awserror.New(code, message) }
func invalid(message string) error {
	e := awserror.New("WAFInvalidParameterException", message)
	e.Fields = map[string]any{"Reason": message}
	return e
}

func missing() error {
	return failure("WAFNonexistentItemException", "Resource does not exist in this account, Region and scope")
}

func locked() error {
	return failure(
		"WAFOptimisticLockException",
		"LockToken is missing or no longer identifies the current configuration",
	)
}

func associated() error {
	return failure("WAFAssociatedItemException", "Resource is referenced by a rule or associated with a resource")
}
func id() string { return uuid.NewV4().String() }

// Only the commercial partition is supported. Validate the region against the
// pinned model's regional endpoint cases, with newer commercial regions included.
var regions = strings.Fields(`af-south-1 ap-east-1 ap-east-2 ap-northeast-1 ap-northeast-2 ap-northeast-3
 ap-south-1 ap-south-2 ap-southeast-1 ap-southeast-2 ap-southeast-3 ap-southeast-4 ap-southeast-5
 ap-southeast-6 ap-southeast-7 ca-central-1 ca-west-1 eu-central-1 eu-central-2 eu-north-1 eu-south-1
 eu-south-2 eu-west-1 eu-west-2 eu-west-3 il-central-1 me-central-1 me-south-1 mx-central-1 sa-east-1
 us-east-1 us-east-2 us-west-1 us-west-2`)

func scopeFor(ctx context.Context, value *string) (request.Scope, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return scope, err
	}
	if !slices.Contains(regions, scope.Region) {
		return scope, invalid("Unsupported commercial Region")
	}
	if value == nil || (*value != regional && *value != cloudfront) {
		return scope, invalid("Scope must be REGIONAL or CLOUDFRONT")
	}
	if *value == cloudfront && scope.Region != "us-east-1" {
		return scope, invalid("CLOUDFRONT scope requires us-east-1")
	}
	return scope, nil
}

func bucket(scope string) string {
	if scope == cloudfront {
		return "global"
	}
	return "regional"
}

func recordKey(scope request.Scope, kind, wafScope, identifier string) string {
	return scope.Key(serviceName, bucket(wafScope)+"/"+kind, identifier)
}

func read(tx storage.ReadTx, scope request.Scope, kind, wafScope string, name, identifier *string) (*record, error) {
	if err := nameOK(name); err != nil {
		return nil, err
	}
	if identifier == nil || !uuidPattern.MatchString(*identifier) {
		return nil, invalid("Id must be a UUID")
	}
	r, ok, err := storage.Read[record](tx, recordKey(scope, kind, wafScope, *identifier))
	if err != nil {
		return nil, err
	}
	if !ok || r.Name != *name {
		return nil, missing()
	}
	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, recordKey(scope, r.Kind, r.Scope, r.ID), r)
}

func records(tx storage.ReadTx, scope request.Scope, kind, wafScope string) ([]*record, error) {
	return storage.Records[record](tx, scope.Prefix(serviceName, bucket(wafScope)+"/"+kind))
}

var (
	wafARN = regexp.MustCompile(
		`^arn:aws:wafv2:([a-z0-9-]+):([0-9]{12}):(regional|global)/(webacl|rulegroup|ipset)/` +
			`([A-Za-z0-9_-]{1,128})/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`,
	)
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func byARN(tx storage.ReadTx, scope request.Scope, value, kind, wafScope string) (*record, error) {
	m := wafARN.FindStringSubmatch(value)
	if m == nil {
		return nil, invalid("Malformed or unsupported WAF ResourceARN")
	}
	actualScope := regional
	if m[3] == "global" {
		actualScope = cloudfront
	}
	if actualScope == cloudfront && m[1] != "us-east-1" {
		return nil, invalid("Global WAF ARN requires us-east-1")
	}
	if kind != "" && m[4] != kind {
		return nil, invalid("Incorrect WAF resource type")
	}
	if m[1] != scope.Region || m[2] != scope.AccountID || (wafScope != "" && actualScope != wafScope) {
		return nil, missing()
	}
	return read(tx, scope, m[4], actualScope, new(m[5]), new(m[6]))
}

func (s *Service) create(ctx context.Context, r *record, tags []api.Tag) (*record, error) {
	scope, err := scopeFor(ctx, new(r.Scope))
	if err != nil {
		return nil, err
	}
	if guardErr := nameOK(new(r.Name)); guardErr != nil {
		return nil, guardErr
	}
	if guardErr := descriptionOK(r.Description); guardErr != nil {
		return nil, guardErr
	}
	r.Tags, err = tagsMap(tags, false)
	if err != nil {
		return nil, err
	}
	r.ID = id()
	r.Token = id()
	r.Created = float64(time.Now().UTC().UnixMilli()) / 1000
	r.ARN = fmt.Sprintf(
		"arn:aws:wafv2:%s:%s:%s/%s/%s/%s",
		scope.Region,
		scope.AccountID,
		bucket(r.Scope),
		r.Kind,
		r.Name,
		r.ID,
	)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, e := records(tx, scope, r.Kind, r.Scope)
		if e != nil {
			return e
		}
		for _, other := range rows {
			if other.Name == r.Name {
				return failure("WAFDuplicateItemException", "Name already exists in this scope")
			}
		}
		if len(rows) >= 100 {
			return failure(
				"WAFLimitsExceededException",
				"At most 100 resources of each type per account, Region and scope",
			)
		}
		if e := stateOK(tx, scope, r); e != nil {
			return e
		}
		return save(tx, scope, r)
	})
	return r, err
}

func (s *Service) get(ctx context.Context, kind string, name, identifier, wafScope *string) (*record, error) {
	scope, err := scopeFor(ctx, wafScope)
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, kind, *wafScope, name, identifier)
		return e
	})
	return r, err
}

func (s *Service) update(
	ctx context.Context,
	kind string,
	name, identifier, wafScope, token *string,
	desired *record,
) (*record, error) {
	scope, err := scopeFor(ctx, wafScope)
	if err != nil {
		return nil, err
	}
	var r *record
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		var e error
		r, e = read(tx, scope, kind, *wafScope, name, identifier)
		if e != nil {
			return e
		}
		if token == nil || *token != r.Token {
			return locked()
		}
		r.Description = desired.Description
		if kind == kindIP {
			r.Addresses = desired.Addresses
		} else {
			r.Rules = desired.Rules
			r.Visibility = desired.Visibility
			if kind == kindACL {
				r.DefaultAction = desired.DefaultAction
			}
		}
		if e := descriptionOK(r.Description); e != nil {
			return e
		}
		if e := stateOK(tx, scope, r); e != nil {
			return e
		}
		r.Token = id()
		return save(tx, scope, r)
	})
	return r, err
}

func (s *Service) remove(ctx context.Context, kind string, name, identifier, wafScope, token *string) error {
	scope, err := scopeFor(ctx, wafScope)
	if err != nil {
		return err
	}
	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, e := read(tx, scope, kind, *wafScope, name, identifier)
		if e != nil {
			return e
		}
		if token == nil || *token != r.Token {
			return locked()
		}
		for _, k := range []string{kindACL, kindGroup} {
			rows, readErr := records(tx, scope, k, r.Scope)
			if readErr != nil {
				return readErr
			}
			for _, other := range rows {
				for _, rule := range other.Rules {
					if reference(rule.Statement) == r.ARN {
						return associated()
					}
				}
			}
		}
		rows, e := storage.Records[association](tx, scope.Prefix(serviceName, "association"))
		if e != nil {
			return e
		}
		for _, a := range rows {
			if a.WebACL == r.ARN {
				return associated()
			}
		}
		tx.Delete(recordKey(scope, kind, r.Scope, r.ID))
		return nil
	})
}

func (s *Service) list(
	ctx context.Context,
	kind string,
	wafScope, token *string,
	limit *int64,
) ([]*record, *string, error) {
	scope, err := scopeFor(ctx, wafScope)
	if err != nil {
		return nil, nil, err
	}
	var rows []*record
	err = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var e error; rows, e = records(tx, scope, kind, *wafScope); return e },
	)
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(rows, func(a, b *record) int { return strings.Compare(a.Name+"/"+a.ID, b.Name+"/"+b.ID) })
	return page(rows, func(r *record) string { return r.Name + "/" + r.ID }, token, limit, scope, kind+"/"+*wafScope)
}

func reference(st *api.Statement) string {
	if st == nil {
		return ""
	}
	if st.IPSetReferenceStatement != nil {
		return common.Value(st.IPSetReferenceStatement.ARN)
	}
	if st.RuleGroupReferenceStatement != nil {
		return common.Value(st.RuleGroupReferenceStatement.ARN)
	}
	return ""
}
