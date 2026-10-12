// Package elb stores Classic Load Balancer metadata: listeners, placement, health check settings, registered
// instances, attributes and tags. Nothing routes traffic or checks instance health.
package elb

import (
	"context"
	"errors"
	"fmt"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "elb"
	balancerKind   = "loadbalancer"
	schemeInternet = "internet-facing"
	schemeInternal = "internal"
	// sourceGroupOwner and sourceGroupName are the source security group of every load balancer.
	sourceGroupOwner = "amazon-elb"
	sourceGroupName  = "amazon-elb-sg"
	digits           = "0123456789"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 records in the service's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(tx storage.ReadTx, scope request.Scope, vpcID string, ids []string) ([]string, error)
	MountInstance(storage.ReadTx, request.Scope, string) (*ec2api.Instance, error)
	Zones(region string) []string
}

type Service struct {
	store   storage.Store
	network Network
}

// balancer is a load balancer's record; Description is what DescribeLoadBalancers returns.
type balancer struct {
	Tags        map[string]string `json:",omitempty"`
	Description api.LoadBalancerDescription
	Attributes  api.LoadBalancerAttributes
}

func (b *balancer) name() string { return common.Value(b.Description.LoadBalancerName) }

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

func invalid(format string, args ...any) error { return awserror.Validation(format, args...) }

func configuration(message string) error {
	return awserror.New("InvalidConfigurationRequestException", message)
}

func notFound(name string) error {
	return awserror.New(
		"AccessPointNotFoundException",
		fmt.Sprintf("There is no ACTIVE Load Balancer named '%s'", name),
	)
}

func arn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:elasticloadbalancing:%s:%s:loadbalancer/%s", scope.Region, scope.AccountID, name)
}

// remap gives an EC2 lookup failure the load balancer error code, and passes anything else through.
func remap(err error, code string) error {
	var e *awserror.Error
	if errors.As(err, &e) {
		return awserror.New(code, e.Message)
	}

	return err
}

func read(tx storage.ReadTx, scope request.Scope, name string) (*balancer, error) {
	b, ok, err := storage.Read[balancer](tx, scope.Key(serviceName, balancerKind, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound(name)
	}

	return b, nil
}

func save(tx storage.WriteTx, scope request.Scope, b *balancer) error {
	return storage.Write(tx, scope.Key(serviceName, balancerKind, b.name()), b)
}

// update reads the named load balancer, lets change edit it, and writes it once, so a rejected change leaves the
// record unchanged.
func (s *Service) update(
	ctx context.Context,
	name *string,
	change func(tx storage.WriteTx, scope request.Scope, b *balancer) error,
) (*balancer, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var out *balancer
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, e := read(tx, scope, common.Value(name))
		if e != nil {
			return e
		}

		if changeErr := change(tx, scope, b); changeErr != nil {
			return changeErr
		}

		if saveErr := save(tx, scope, b); saveErr != nil {
			return saveErr
		}

		out = b
		return nil
	})

	return out, err
}
