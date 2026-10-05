// Package eks implements the Amazon EKS control plane API without HTTP
// dependencies. Without a Runtime no Kubernetes API server runs: clusters, node
// groups, Fargate profiles, and add-ons settle in ACTIVE at once, and every
// update succeeds when it is made. With one (UseRuntime), clusters start
// CREATING and a runner starts their API servers (runtime.go), which the
// gateway serves through APIServer. Resources belong to an account and region:
//
//	<scope>/eks/cluster/<name>
//	<scope>/eks/<kind>/<cluster>/<name>   (nodegroup, fargate, addon, pod-identity by ID)
//	<scope>/eks/access/<cluster>/<path-escaped principal ARN>
//	<scope>/eks/update/<cluster>/<update ID>
//	aws/eks/index/<endpoint ID>          (clusters the runner owns)
//
// Subnets and security groups are EC2's, checked through ec2.Service.Place.
package eks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindCluster   = "cluster"
	kindNodegroup = "nodegroup"
	kindFargate   = "fargate"
	kindAddon     = "addon"
	kubeSystem    = "kube-system"
	service       = "eks"
	active        = "ACTIVE"
	deleting      = "DELETING"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store storage.Store
	// network places clusters and node groups in EC2's subnets.
	network *ec2.Service
	now     func() time.Time
	// runner runs API servers once UseRuntime is called.
	runner *runner
}

func New(store storage.Store, network *ec2.Service) *Service {
	return &Service{store: store, network: network, now: time.Now}
}

func invalidParameter(format string, args ...any) error {
	return awserror.New("InvalidParameterException", fmt.Sprintf(format, args...))
}

func invalidRequest(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

func inUse(format string, args ...any) error {
	return awserror.New("ResourceInUseException", fmt.Sprintf(format, args...))
}

// owned is the key of a record of kind that a cluster owns.
func owned(scope request.Scope, kind, clusterName, id string) string {
	return scope.Prefix(service, kind) + clusterName + "/" + id
}

func arn(scope request.Scope, resource string) string {
	return "arn:aws:eks:" + scope.Region + ":" + scope.AccountID + ":" + resource
}

// page returns the items after the one token names, at most limit of them
// (1 to 100, 100 by default), and the token for the rest, if any.
func page[T any](items []T, id func(T) string, token *string, limit *int64) (selected []T, next *string, err error) {
	n := int64(100)
	if limit != nil {
		if *limit < 1 || *limit > 100 {
			return nil, nil, invalidParameter("maxResults must be between 1 and 100.")
		}

		n = *limit
	}

	return common.Page(items, id, token, int(n), invalidParameter("The pagination token is invalid."))
}

var namePattern = regexp.MustCompile(`^[0-9A-Za-z][A-Za-z0-9\-_]*$`)

func checkName(name *string, member string, maxLength int) error {
	if name == nil || len(*name) > maxLength || !namePattern.MatchString(*name) {
		return invalidParameter("%s must be 1 to %d letters, digits, hyphens, and underscores, starting with a "+
			"letter or digit.", member, maxLength)
	}

	return nil
}

var roleARN = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/.+$`)

func checkRole(role *string, member string) error {
	if role == nil || !roleARN.MatchString(*role) {
		return invalidParameter("%s must be an IAM role ARN.", member)
	}

	return nil
}

// tagProblem reports how tags break the tag rules, if they do: the shared
// limits, and keys starting with "aws:".
func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return invalidParameter("%s", err.Error())
	}

	return nil
}

// place checks subnets and security groups in EC2, reporting EC2's errors as
// EKS's.
func (s *Service) place(ctx context.Context, scope request.Scope, subnets, groups []string) (ec2.Placement, error) {
	p, err := s.network.Place(ctx, scope, subnets, groups)
	var e *awserror.Error
	if errors.As(err, &e) {
		return p, invalidParameter("%s", e.Message)
	}

	return p, err
}
