// Package shield stores global Shield Advanced configuration without attack mitigation.
package shield

import (
	"context"
	"fmt"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName        = "shield"
	patternArbitrary   = "ARBITRARY"
	kindProtection     = "protection"
	kindGroup          = "protection-group"
	resourceHostedZone = "ROUTE_53_HOSTED_ZONE"
	route53Service     = "route53"
)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
}

func New(store storage.Store) *Service         { return &Service{store: store} }
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

type subscriptionRecord struct {
	Subscription api.Subscription
	Contacts     []api.EmergencyContact
	Role         *string
	Buckets      []string
	Initialized  bool
}

type record struct {
	Protection   *api.Protection
	Group        *api.ProtectionGroup
	Tags         map[string]string
	ResourceType string
}

func invalid(message string) error { return awserror.New("InvalidParameterException", message) }
func missing() error {
	return awserror.New("ResourceNotFoundException", "Shield resource or subscription does not exist")
}

func already() error {
	return awserror.New("ResourceAlreadyExistsException", "Shield resource already exists")
}

func noChange() error {
	return awserror.New("InvalidOperationException", "Operation does not change the current state")
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:shield::%s:%s/%s", scope.AccountID, kind, id)
}

func subscription(tx storage.ReadTx, scope request.Scope) (*subscriptionRecord, error) {
	r, ok, err := storage.Read[subscriptionRecord](tx, scope.Key(serviceName, "subscription", "default"))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func (s *Service) transaction(
	ctx context.Context,
	f func(storage.WriteTx, request.Scope, *subscriptionRecord) error,
) error {
	scope, err := request.GlobalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := subscription(tx, scope)
		if err != nil {
			return err
		}

		if err := f(tx, scope, r); err != nil {
			return err
		}

		return storage.Write(tx, scope.Key(serviceName, "subscription", "default"), r)
	})
}

func read(tx storage.ReadTx, scope request.Scope, kind, id string) (*record, error) {
	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func pageSize(p *int64) int {
	if p == nil || *p == 0 {
		return 100
	}

	return int(*p)
}

func matches(values []string, v *string) bool {
	for _, x := range values {
		if x == common.Value(v) {
			return true
		}
	}

	return len(values) == 0
}

// reference runs the owning service's read API outside Shield's transaction.
func (s *Service) reference(ctx context.Context, value, kind string) (string, error) {
	parts := strings.SplitN(value, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" {
		return "", invalid("Invalid resource ARN")
	}

	m, err := request.Global(ctx)
	if err != nil {
		return "", err
	}

	if parts[4] != "" && parts[4] != m.AccountID {
		return "", missing()
	}

	if parts[3] != "" {
		m.Region = parts[3]
	}

	ctx = request.WithMetadata(ctx, m)
	service, op, field, id, resourceType := "", "", "", parts[5], ""
	switch {
	case kind == "health" && parts[2] == route53Service && strings.HasPrefix(id, "healthcheck/"):
		service, op, field, id = route53Service, "GetHealthCheck", "HealthCheckId", strings.TrimPrefix(
			id,
			"healthcheck/",
		)
	case kind == "role" && parts[2] == "iam" && strings.HasPrefix(id, "role/"):
		service, op, field, id = "iam", "GetRole", "RoleName", id[strings.LastIndex(id, "/")+1:]
	case kind == kindProtection && parts[2] == "cloudfront" && strings.HasPrefix(id, "distribution/"):
		service, op, field, id, resourceType = "cloudfront", "GetDistribution", "Id", strings.TrimPrefix(
			id,
			"distribution/",
		), "CLOUDFRONT_DISTRIBUTION"
	case kind == kindProtection && parts[2] == route53Service && strings.HasPrefix(id, "hostedzone/"):
		service, op, field, id, resourceType = route53Service, "GetHostedZone", "Id", strings.TrimPrefix(
			id,
			"hostedzone/",
		), resourceHostedZone
	case kind == kindProtection && parts[2] == "globalaccelerator" && strings.HasPrefix(id, "accelerator/"):
		service, op, field, id, resourceType = "globalaccelerator", "DescribeAccelerator", "AcceleratorArn", value,
			"GLOBAL_ACCELERATOR"
	case kind == kindProtection && parts[2] == "elasticloadbalancing" && strings.HasPrefix(id, "loadbalancer/"):
		rest := strings.TrimPrefix(id, "loadbalancer/")
		if strings.HasPrefix(rest, "app/") {
			service, op, field, id, resourceType = "elbv2", "DescribeLoadBalancers", "LoadBalancerArns", value,
				"APPLICATION_LOAD_BALANCER"
		} else if !strings.Contains(rest, "/") {
			service, op, field, id, resourceType = "elb", "DescribeLoadBalancers", "LoadBalancerNames", rest,
				"CLASSIC_LOAD_BALANCER"
		}

	case kind == kindProtection && parts[2] == "ec2" && strings.HasPrefix(id, "eip-allocation/"):
		service, op, field, id, resourceType = "ec2", "DescribeAddresses", "AllocationIds", strings.TrimPrefix(
			id,
			"eip-allocation/",
		), "ELASTIC_IP_ALLOCATION"
	}

	if service == "" {
		return "", invalid("Unsupported Shield resource ARN")
	}

	if s.caller == nil {
		return "", missing()
	}

	var param any = id
	if field == "LoadBalancerArns" || field == "LoadBalancerNames" || field == "AllocationIds" {
		param = []any{id}
	}

	_, err = s.caller.Call(ctx, service, op, map[string]any{field: param})
	if err != nil {
		return "", missing()
	}

	return resourceType, nil
}

var _ api.API = (*Service)(nil)
