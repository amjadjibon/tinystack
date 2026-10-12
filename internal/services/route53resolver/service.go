// Package route53resolver stores endpoint, rule and association metadata. It does
// not allocate network interfaces or addresses, or forward DNS queries.
package route53resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	outbound        = "OUTBOUND"
	serviceName     = "route53resolver"
	endpointKind    = "resolver-endpoint"
	ruleKind        = "resolver-rule"
	associationKind = "resolver-rule-association"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 records within the caller's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

type Service struct {
	store   storage.Store
	network Network
}

type record struct {
	Endpoint      *api.ResolverEndpoint
	Rule          *api.ResolverRule
	Association   *api.ResolverRuleAssociation
	Tags          map[string]string
	CreateRequest string
	ID            string
	Kind          string
	Ips           []api.IpAddressRequest
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

func invalid(message string) error { return awserror.New("InvalidParameterException", message) }
func missing() error {
	return awserror.New("ResourceNotFoundException", "Resolver resource does not exist in this account and Region")
}

func exists() error {
	return awserror.New("ResourceExistsException", "Resolver resource already exists")
}

func read(tx storage.ReadTx, scope request.Scope, kind string, id *string) (*record, error) {
	if err := checkString(id, 1, 64, "Resource ID"); err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, *id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:route53resolver:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

// tokenRecord keeps the original request separate from mutable configuration.
// The existing resource is returned after retries, even after a later update.
func tokenRecord(
	tx storage.ReadTx,
	scope request.Scope,
	kind string,
	token *string,
	input any,
) (*record, string, error) {
	if err := checkString(token, 1, 255, "CreatorRequestId"); err != nil {
		return nil, "", err
	}

	data, err := json.Marshal(input)
	if err != nil {
		return nil, "", invalid("Invalid create request")
	}

	records, err := storage.Records[record](tx, scope.Prefix(serviceName, kind))
	if err != nil {
		return nil, "", err
	}

	for _, r := range records {
		current := r.creatorToken()
		if current == *token {
			if r.CreateRequest != string(data) {
				return nil, "", exists()
			}

			return r, string(data), nil
		}
	}

	return nil, string(data), nil
}

func (r *record) creatorToken() string {
	if r.Endpoint != nil {
		return common.Value(r.Endpoint.CreatorRequestId)
	}

	return common.Value(r.Rule.CreatorRequestId)
}

func (s *Service) get(ctx context.Context, kind string, id *string) (*record, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var r *record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		r, e = read(tx, scope, kind, id)
		return e
	})
	return r, err
}

func (s *Service) list(ctx context.Context, kind string, token *string, maximum *int64) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit, err := pageSize(maximum)
	if err != nil {
		return nil, nil, err
	}

	var records []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		records, e = storage.Records[record](tx, scope.Prefix(serviceName, kind))
		return e
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(records, func(r *record) string { return r.ID }, token, limit,
		awserror.New("InvalidNextTokenException", "Invalid NextToken"))
}

func resourceReference(scope request.Scope, value *string) (kind, id string, err error) {
	if err := checkString(value, 1, 255, "ResourceArn"); err != nil {
		return "", "", err
	}

	tail, ok := strings.CutPrefix(*value, fmt.Sprintf("arn:aws:route53resolver:%s:%s:", scope.Region, scope.AccountID))
	if !ok {
		return "", "", missing()
	}

	kind, id, ok = strings.Cut(tail, "/")
	if !ok || (kind != endpointKind && kind != ruleKind) || strings.ContainsAny(id, "/:") {
		return "", "", missing()
	}

	return kind, id, nil
}
