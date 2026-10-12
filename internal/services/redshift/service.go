// Package redshift stores provisioned cluster, subnet group and parameter group
// metadata. Endpoints and available status do not represent a running database.
package redshift

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName      = "redshift"
	clusterKind      = "cluster"
	subnetKind       = "subnetgroup"
	parameterKind    = "parametergroup"
	defaultParameter = "default.redshift-1.0"
	family           = "redshift-1.0"
	available        = "available"
	multiNode        = "multi-node"
	singleNode       = "single-node"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 subnet records in the service's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
}

type Service struct {
	store   storage.Store
	network Network
}

type record struct {
	Cluster     *api.Cluster
	Subnet      *api.ClusterSubnetGroup
	Parameter   *api.ClusterParameterGroup
	Tags        map[string]string
	Name        string
	Kind        string
	ClusterType string
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

func invalid(message string) error { return awserror.Validation("%s", message) }

func resourceError(kind, suffix string) error {
	code := "Cluster" + suffix
	if kind == subnetKind {
		code = "ClusterSubnetGroup" + suffix
	}

	if kind == parameterKind {
		code = "ClusterParameterGroup" + suffix
	}

	if kind == subnetKind && suffix == "NotFound" {
		code += "Fault"
	}

	e := awserror.New(code, "Redshift resource "+suffix)
	if suffix == "NotFound" && kind != subnetKind {
		e.StatusCode = 404
	}

	return e
}

func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:redshift:%s:%s:%s:%s", scope.Region, scope.AccountID, kind, name)
}

func defaultGroup() *record {
	return &record{
		Name: defaultParameter, Kind: parameterKind, Tags: map[string]string{},
		Parameter: &api.ClusterParameterGroup{
			ParameterGroupName: new(defaultParameter),
			ParameterGroupFamily: new(
				family,
			),
			Description: new("Default Redshift 1.0 parameter group"),
			Tags:        []api.Tag{},
		},
	}
}

func read(tx storage.ReadTx, scope request.Scope, kind, name string) (*record, error) {
	if kind != clusterKind {
		name = strings.ToLower(name)
	}
	if kind == parameterKind && name == defaultParameter {
		return defaultGroup(), nil
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, resourceError(kind, "NotFound")
	}

	return r, nil
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	tags := tagList(r.Tags)
	switch r.Kind {
	case clusterKind:
		r.Cluster.Tags = tags
	case subnetKind:
		r.Subnet.Tags = tags
	case parameterKind:
		r.Parameter.Tags = tags
	}

	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.Name), r)
}

func insert(tx storage.WriteTx, scope request.Scope, r *record) error {
	if _, exists := tx.Get(scope.Key(serviceName, r.Kind, r.Name)); exists ||
		r.Kind == parameterKind && r.Name == defaultParameter {
		return resourceError(r.Kind, "AlreadyExists")
	}

	return save(tx, scope, r)
}

func (s *Service) list(
	ctx context.Context,
	kind string,
	name, marker *string,
	maximum *int64,
) ([]*record, *string, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, nil, err
	}

	limit, err := pageSize(maximum)
	if err != nil {
		return nil, nil, err
	}

	var items []*record
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if name != nil {
			if *name == "" {
				return invalid("Resource name cannot be empty")
			}

			r, e := read(tx, scope, kind, *name)
			if e != nil {
				return e
			}

			items = append(items, r)
			return nil
		}

		var e error
		items, e = storage.Records[record](tx, scope.Prefix(serviceName, kind))
		if e != nil {
			return e
		}

		if kind == parameterKind {
			// Merge the virtual built-in into the already sorted storage records.
			i := 0
			for i < len(items) && items[i].Name < defaultParameter {
				i++
			}

			items = append(items, nil)
			copy(items[i+1:], items[i:])
			items[i] = defaultGroup()
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	return common.Page(items, func(r *record) string { return r.Name }, marker, limit, invalid("Invalid Marker"))
}

func resourceReference(scope request.Scope, value *string) (kind, name string, err error) {
	prefix := fmt.Sprintf("arn:aws:redshift:%s:%s:", scope.Region, scope.AccountID)
	tail, ok := strings.CutPrefix(common.Value(value), prefix)
	if !ok {
		return "", "", awserror.New("ResourceNotFoundFault", "Resource is not in this account and Region")
	}

	kind, name, ok = strings.Cut(tail, ":")
	if !ok || name == "" || strings.ContainsAny(name, "/:") ||
		(kind != clusterKind && kind != subnetKind && kind != parameterKind) {
		return "", "", invalid("Invalid Redshift resource ARN")
	}

	return kind, name, nil
}
