// Package bedrockagentcorecontrol implements the Amazon Bedrock AgentCore control plane: agent runtimes with their
// versions and endpoints, and short-term memories. Resources are records whose status settles in the background.
//
// Records are JSON under <account>/<region>/bedrockagentcorecontrol/:
//
//	runtime/<id>               a runtime and its tags
//	version/<id>/<number>      one immutable runtime version, the number zero-padded to five digits
//	endpoint/<id>/<name>       an endpoint, including the DEFAULT one AWS creates
//	memory/<id>                a memory and its tags
//
// The data plane (package bedrockagentcore) reads versions, endpoints and memories through Target and Memory.
package bedrockagentcorecontrol

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagentcorecontrol"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName  = "bedrockagentcorecontrol"
	kindRuntime  = "runtime"
	kindVersion  = "version"
	kindEndpoint = "endpoint"
	kindMemory   = "memory"
	indexPrefix  = "aws/bedrockagentcorecontrol/index/"

	// DefaultEndpoint is the endpoint AWS creates with every runtime; it follows the latest version.
	DefaultEndpoint = "DEFAULT"

	statusCreating = "CREATING"
	statusUpdating = "UPDATING"
	statusReady    = "READY"
	statusDeleting = "DELETING"
	statusActive   = "ACTIVE"

	// transitionDelay is how long a resource stays in a transitional status before the worker settles it.
	transitionDelay = 200 * time.Millisecond
	// listPageSize is the page size of a list that the caller does not limit.
	listPageSize = 100

	// Lifecycle defaults of the runtime service contract, in seconds.
	defaultIdleTimeout = 900
	defaultMaxLifetime = 28800

	protocolHTTP = "HTTP"
	protocolMCP  = "MCP"
	protocolA2A  = "A2A"

	arnPrefix = "arn:aws:bedrock-agentcore:"
)

var _ api.API = (*Service)(nil)

// Network reads existing EC2 records inside the caller's transaction.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

// Observer hears of deletions after they commit, so the data plane can drop what depends on them.
type Observer interface {
	// RuntimeDeleted reports that a runtime and so all its endpoints and versions are going away.
	RuntimeDeleted(ctx context.Context, scope request.Scope, runtimeID string)
	// EndpointDeleted reports that an endpoint is going away.
	EndpointDeleted(ctx context.Context, scope request.Scope, runtimeID, endpoint string)
	// VersionDeleted reports that one version of a runtime is gone.
	VersionDeleted(ctx context.Context, scope request.Scope, runtimeID string, version int)
	// MemoryDeleted reports that a memory is going away.
	MemoryDeleted(ctx context.Context, scope request.Scope, memoryID string)
}

// Service stores AgentCore runtimes and memories and settles their statuses in the background.
type Service struct {
	store    storage.Store
	network  Network
	roles    *iam.Service
	keys     *kms.Service
	workers  *background.Group
	observer Observer
}

// New constructs the service and starts its status worker; Stop ends it.
func New(store storage.Store, network Network, roles *iam.Service, keys *kms.Service) *Service {
	s := &Service{store: store, network: network, roles: roles, keys: keys, workers: background.New()}
	s.workers.Every(100*time.Millisecond, s.advance)

	return s
}

// UseObserver has o told about deletions. Call it before serving requests.
func (s *Service) UseObserver(o Observer) { s.observer = o }

// Stop waits for the status worker to finish its current pass.
func (s *Service) Stop() { s.workers.Stop() }

func invalid(format string, args ...any) error { return awserror.Validation(format, args...) }

func conflict(format string, args ...any) error {
	return awserror.New("ConflictException", fmt.Sprintf(format, args...))
}

func notFound(format string, args ...any) error {
	return awserror.New("ResourceNotFoundException", fmt.Sprintf(format, args...))
}

var errBadToken = awserror.Validation("nextToken is not valid")

func pageLimit(n *int64) int {
	if n == nil {
		return listPageSize
	}

	return int(*n)
}

func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
}

func (s *Service) view(ctx context.Context, fn func(storage.ReadTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	return s.store.View(ctx, func(tx storage.ReadTx) error { return fn(tx, scope) })
}

// work indexes a resource whose status the worker has yet to settle.
type work struct {
	Scope request.Scope
	Kind  string
	ID    string
}

func indexKey(scope request.Scope, kind, id string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + kind + "/" + id
}

// queue stores r and indexes it for the worker to settle once its Due time passes.
func queue(tx storage.WriteTx, scope request.Scope, kind, id, key string, r any) error {
	if err := storage.Write(tx, key, r); err != nil {
		return err
	}

	return storage.Write(tx, indexKey(scope, kind, id), work{Scope: scope, Kind: kind, ID: id})
}

func runtimeKey(scope request.Scope, id string) string {
	return scope.Key(serviceName, kindRuntime, id)
}

func versionKey(scope request.Scope, id string, number int) string {
	return scope.Key(serviceName, kindVersion, id+"/"+fmt.Sprintf("%05d", number))
}

func versionPrefix(scope request.Scope, id string) string {
	return scope.Prefix(serviceName, kindVersion) + id + "/"
}

func endpointKey(scope request.Scope, id, name string) string {
	return scope.Key(serviceName, kindEndpoint, id+"/"+name)
}

func endpointPrefix(scope request.Scope, id string) string {
	return scope.Prefix(serviceName, kindEndpoint) + id + "/"
}

func memoryKey(scope request.Scope, id string) string { return scope.Key(serviceName, kindMemory, id) }

func runtimeARN(scope request.Scope, id string) string {
	return arnPrefix + scope.Region + ":" + scope.AccountID + ":runtime/" + id
}

func endpointARN(scope request.Scope, id, name string) string {
	return runtimeARN(scope, id) + "/runtime-endpoint/" + name
}

func memoryARN(scope request.Scope, id string) string {
	return arnPrefix + scope.Region + ":" + scope.AccountID + ":memory/" + id
}

// newID returns a resource ID: the name and ten random alphanumeric characters, as AWS generates them.
func newID(name string) string { return name + "-" + common.RandomID(10, common.AlphabetAlphanumeric) }

// parseVersion reads a runtime version, such as "3".
func parseVersion(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, invalid("agentRuntimeVersion %q is not a version number", v)
	}

	return n, nil
}

func version(n int) *string { return new(strconv.Itoa(n)) }
