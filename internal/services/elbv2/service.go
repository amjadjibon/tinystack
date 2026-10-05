// Package elbv2 implements Elastic Load Balancing (application and network
// load balancers) without HTTP dependencies. Resources belong to an account
// and region:
//
//	<scope>/elbv2/lb/<name>                              (settings, attributes, and tags)
//	<scope>/elbv2/tg/<name>                              (settings, attributes, tags, and targets)
//	<scope>/elbv2/listener/<load balancer>/<listener ID>
//	<scope>/elbv2/rule/<listener ID>/<rule ID>           (a listener's default rule is its record)
//	aws/elbv2/dns/<DNS label>                            (the account, region, and name of each load balancer)
//
// Subnets, security groups, VPCs, instances, certificates, and functions are
// checked in-process through a catalog.Caller. Application load balancers
// also serve HTTP: the gateway sends requests to a load balancer's DNS name
// (or /_aws/elb/<DNS label>/...) to Execute, which evaluates a listener's
// rules and forwards to Lambda functions or IP targets (serve.go).
package elbv2

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service         = "elbv2"
	kindLB          = "lb"
	kindTG          = "tg"
	kindListener    = "listener"
	kindRule        = "rule"
	dnsPrefix       = "aws/" + service + "/dns/"
	defaultPageSize = 400
	typeApplication = "application"
	typeNetwork     = "network"
	stateActive     = "active"
	// memberName is the Name member of EC2 filters and states.
	memberName = "Name"
	// memberValues is the Values member of EC2 filters.
	memberValues = "Values"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	logger *slog.Logger
	now    func() time.Time
	// turn picks IP targets in turn.
	turn atomic.Uint64
}

// New returns an Elastic Load Balancing service.
func New(store storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{store: store, logger: logger, now: time.Now}
}

// UseCaller lets the service check what requests name in other services and
// invoke Lambda targets. Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

func identity(ctx context.Context) (request.Metadata, error) {
	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return request.Metadata{}, fmt.Errorf("missing request identity")
	}

	return m, nil
}

func key(scope request.Scope, kind, name string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/" + name
}

func arn(scope request.Scope, resource string) string {
	return "arn:aws:elasticloadbalancing:" + scope.Region + ":" + scope.AccountID + ":" + resource
}

// resourceID returns 16 random hex digits, the last part of every ARN.
func resourceID() string { return common.RandomID(16) }

func validation(format string, args ...any) error {
	return awserror.New("ValidationError", fmt.Sprintf(format, args...))
}

// arnParts returns the parts of an ELB ARN's resource after its type:
// "loadbalancer/app/<name>/<ID>" gives ["app", <name>, <ID>].
func arnParts(value, kind string, scope request.Scope) ([]string, bool) {
	rest, ok := strings.CutPrefix(value, arn(scope, kind+"/"))
	if !ok {
		return nil, false
	}

	return strings.Split(rest, "/"), true
}

// page returns the items after the one the Marker names, at most PageSize of
// them. keyOf must give each item a different key.
func page[T any](items []T, keyOf func(T) string, marker *string, pageSize *int64) (
	selected []T, next *string, err error,
) {
	limit := int64(defaultPageSize)
	if pageSize != nil {
		limit = *pageSize
	}

	if limit < 1 || limit > defaultPageSize {
		return nil, nil, validation("PageSize must be 1 to 400.")
	}

	start := 0
	if marker != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*marker)
		if decodeErr != nil {
			return nil, nil, validation("The marker '%s' is not valid.", *marker)
		}

		start = slices.IndexFunc(items, func(item T) bool { return keyOf(item) == string(after) }) + 1
		if start == 0 {
			return nil, nil, validation("The marker '%s' is not valid.", *marker)
		}
	}

	items = items[start:]
	if int64(len(items)) <= limit {
		return items, nil, nil
	}

	items = items[:limit]
	return items, new(base64.RawURLEncoding.EncodeToString([]byte(keyOf(items[len(items)-1])))), nil
}

// tagMap validates request tags.
func tagMap(tags []api.Tag) (map[string]string, error) {
	out := map[string]string{}
	for _, t := range tags {
		k := common.Value(t.Key)
		if _, dup := out[k]; dup {
			return nil, awserror.New("DuplicateTagKeysException", "Tag key '"+k+"' is duplicated.")
		}

		out[k] = common.Value(t.Value)
	}

	return out, checkTags(out)
}

func checkTags(tags map[string]string) error {
	switch err := tagging.Check(tags); {
	case errors.Is(err, tagging.ErrLimit):
		return awserror.New("TooManyTagsException", "The quota for the number of tags per resource was reached.")
	case err != nil:
		return validation("%s", err.Error())
	}

	return nil
}

func tagList(tags map[string]string) []api.Tag {
	out := []api.Tag{}
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		out = append(out, api.Tag{Key: new(k), Value: new(tags[k])})
	}

	return out
}

// callContext is the context of an in-process call to another service.
func callContext(ctx context.Context, scope request.Scope, target string) context.Context {
	return request.WithMetadata(context.WithoutCancel(ctx),
		request.Metadata{Scope: scope, Service: target, RequestID: resourceID() + resourceID()})
}

// call runs an operation of another service in-process.
func (s *Service) call(ctx context.Context, scope request.Scope, target, op string, params map[string]any) (
	map[string]any, error,
) {
	if s.caller == nil {
		return nil, awserror.New("InternalFailure", "Elastic Load Balancing has no caller")
	}

	return s.caller.Call(callContext(ctx, scope, target), target, op, params)
}

// objects returns a result member as a list of objects.
func objects(out map[string]any, name string) []map[string]any {
	items, _ := out[name].([]any)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if o, ok := item.(map[string]any); ok {
			result = append(result, o)
		}
	}

	return result
}

func text(o map[string]any, name string) string {
	s, _ := o[name].(string)
	return s
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}

	return out
}
