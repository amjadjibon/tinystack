// Package elasticache implements the Amazon ElastiCache service. Without a
// Runtime, caches run in records mode: clusters and parameter groups settle in
// available at once. With a Docker runtime (UseRuntime), cache nodes run as
// containers whose ports map to local ports.
package elasticache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "elasticache"

	statusAvailable = "available"
	defaultVpcID    = "vpc-default"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store    storage.Store
	caller   *catalog.Caller
	logger   *slog.Logger
	now      func() time.Time
	eventSeq atomic.Uint64
}

// New returns an ElastiCache service using store for persistence.
func New(store storage.Store) *Service {
	return &Service{
		store:  store,
		logger: slog.Default(),
		now:    time.Now,
	}
}

// UseCaller lets ElastiCache verify subnets and security groups with EC2.
func (s *Service) UseCaller(caller *catalog.Caller) {
	s.caller = caller
}

// Stop shuts down any background workers.
func (s *Service) Stop() {}

func invalidParameterValue(format string, args ...any) error {
	return awserror.New("InvalidParameterValueException", fmt.Sprintf(format, args...))
}

func invalidParameterCombination(format string, args ...any) error {
	return awserror.New("InvalidParameterCombinationException", fmt.Sprintf(format, args...))
}

func clusterNotFound(id string) error {
	return awserror.New("CacheClusterNotFoundFault", fmt.Sprintf("CacheCluster %s not found.", id))
}

func clusterAlreadyExists(id string) error {
	return awserror.New("CacheClusterAlreadyExistsFault", fmt.Sprintf("Cache cluster %s already exists.", id))
}

func subnetGroupNotFound(name string) error {
	return awserror.New("CacheSubnetGroupNotFoundFault", fmt.Sprintf("CacheSubnetGroup %s not found.", name))
}

func subnetGroupAlreadyExists(name string) error {
	return awserror.New(
		"CacheSubnetGroupAlreadyExistsFault",
		fmt.Sprintf("Cache subnet group %s already exists.", name),
	)
}

func subnetGroupInUse(name string) error {
	msg := fmt.Sprintf("Cache subnet group %s is being used by existing cache clusters.", name)
	return awserror.New("CacheSubnetGroupInUse", msg)
}

func parameterGroupNotFound(name string) error {
	return awserror.New("CacheParameterGroupNotFoundFault", fmt.Sprintf("CacheParameterGroup %s not found.", name))
}

func parameterGroupAlreadyExists(name string) error {
	return awserror.New(
		"CacheParameterGroupAlreadyExistsFault",
		fmt.Sprintf("Cache parameter group %s already exists.", name),
	)
}

func invalidParameterGroupState(format string, args ...any) error {
	return awserror.New("InvalidCacheParameterGroupStateFault", fmt.Sprintf(format, args...))
}

func invalidSubnet(format string, args ...any) error {
	return awserror.New("InvalidSubnet", fmt.Sprintf(format, args...))
}

func invalidARN(arn string) error {
	return awserror.New("InvalidARNFault", fmt.Sprintf("Invalid ARN %s", arn))
}

func tagQuotaExceeded() error {
	return awserror.New(
		"TagQuotaPerResourceExceeded",
		"The request would cause the number of tags on the resource to exceed the limit.",
	)
}

func clusterKey(scope request.Scope, id string) string {
	return scope.Key(serviceName, "cluster", strings.ToLower(id))
}

func subnetGroupKey(scope request.Scope, name string) string {
	return scope.Key(serviceName, "subnetgroup", strings.ToLower(name))
}

func parameterGroupKey(scope request.Scope, name string) string {
	return scope.Key(serviceName, "parametergroup", strings.ToLower(name))
}

func clusterARN(scope request.Scope, id string) string {
	return fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", scope.Region, scope.AccountID, strings.ToLower(id))
}

func subnetGroupARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:elasticache:%s:%s:subnetgroup:%s", scope.Region, scope.AccountID, strings.ToLower(name))
}

func parameterGroupARN(scope request.Scope, name string) string {
	return fmt.Sprintf(
		"arn:aws:elasticache:%s:%s:parametergroup:%s",
		scope.Region,
		scope.AccountID,
		strings.ToLower(name),
	)
}

func eventKey(scope request.Scope, t time.Time, seq uint64) string {
	return scope.Key(serviceName, "event", fmt.Sprintf("%020d-%010d", t.UnixNano(), seq))
}

func (s *Service) call(
	ctx context.Context,
	service, operation string,
	params map[string]any,
) (map[string]any, error) {
	if s.caller == nil {
		return map[string]any{}, nil
	}

	return s.caller.Call(ctx, service, operation, params)
}

func anyList[T any](items []T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}

	return out
}

func objects(m map[string]any, key string) []map[string]any {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}

	var res []map[string]any
	for _, item := range raw {
		if obj, ok := item.(map[string]any); ok {
			res = append(res, obj)
		}
	}

	return res
}

func text(m map[string]any, key string) string {
	val, ok := m[key].(string)
	if !ok {
		return ""
	}

	return val
}
