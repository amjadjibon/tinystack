// Package memorydb stores MemoryDB resources and optionally runs cluster-protocol Valkey or Redis OSS engines.
package memorydb

import (
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName     = "memorydb"
	kindCluster     = "cluster"
	kindUser        = "user"
	kindACL         = "acl"
	kindSubnetGroup = "subnetgroup"

	statusAvailable = "available"
	statusCreating  = "creating"
	statusActive    = "active"
	statusDeleting  = "deleting"

	// The built-in user and ACL every account has; the default user can only belong to the open-access ACL.
	defaultUserName = "default"
	openAccessACL   = "open-access"

	// Documented in the ACL guide: access strings take `on`/`off` and the default user may use everything.
	defaultAccessString = "on ~* &* +@all"

	// The first engine version with ACLs, from the delete-acl example in the ACL guide.
	minimumEngineVersion = "6.2"
)

var _ api.API = (*Service)(nil)

// Network reads scoped EC2 subnet records in the service's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
}

type Service struct {
	store       storage.Store
	network     Network
	runtime     Runtime
	bg          *background.Group
	logger      *slog.Logger
	owner       string
	mu          sync.Mutex // serializes the reconciler with Reset; API calls rely on transactions
	orphansDone bool
}

func New(store storage.Store, network Network) *Service {
	return &Service{store: store, network: network}
}

func fault(code, format string, args ...any) error {
	return awserror.New(code, fmt.Sprintf(format, args...))
}

func invalidValue(format string, args ...any) error {
	return fault("InvalidParameterValueException", format, args...)
}

func invalidCombination(format string, args ...any) error {
	return fault("InvalidParameterCombinationException", format, args...)
}

func key(scope request.Scope, kind, name string) string {
	return scope.Key(serviceName, kind, name)
}

func arn(scope request.Scope, kind, name string) string {
	return fmt.Sprintf("arn:aws:memorydb:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, name)
}

// optional returns nil for an empty value, so unset members stay out of responses.
func optional(v string) *string {
	if v == "" {
		return nil
	}

	return &v
}

// checkName applies the naming rules of the MemoryDB guide, shared by clusters, users and ACLs: 1 to 40
// alphanumeric characters or hyphens, a letter first, no two hyphens in a row and no trailing hyphen.
func checkName(what, name string) error {
	valid := name != "" && len(name) <= 40 && !strings.Contains(name, "--") && !strings.HasSuffix(name, "-")
	for i, c := range name {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		valid = valid && (letter || i > 0 && (c >= '0' && c <= '9' || c == '-'))
	}

	if !valid {
		return invalidValue(
			"%s must be 1 to 40 alphanumeric characters or hyphens, begin with a letter, not contain two consecutive "+
				"hyphens and not end with a hyphen.", what)
	}

	return nil
}

// names are stored lower-case, as the CloudFormation reference documents for ACLs.
func normalize(name *string) string {
	if name == nil {
		return ""
	}

	return strings.ToLower(*name)
}

// pageSize reads MaxResults. The model documents no default or maximum, so an absent value returns everything.
func pageSize(maxResults *int64) (int, error) {
	if maxResults == nil {
		return math.MaxInt, nil
	}

	if *maxResults < 1 {
		return 0, invalidValue("MaxResults must be at least 1.")
	}

	return int(*maxResults), nil
}

func invalidToken() error { return invalidValue("NextToken is not valid.") }
