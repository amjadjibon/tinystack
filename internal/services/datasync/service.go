// Package datasync implements DataSync records and local S3-to-S3 task execution.
package datasync

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const serviceName = "datasync"

// Service stores DataSync configuration and executes S3 transfers.
type Service struct {
	store   storage.Store
	buckets *s3.Service
	roles   *iam.Service
	caller  *catalog.Caller
	workers *background.Group
}

// New constructs a DataSync service. UseCaller starts its persistent work queue.
func New(store storage.Store, buckets *s3.Service, roles *iam.Service) *Service {
	return &Service{store: store, buckets: buckets, roles: roles, workers: background.New()}
}

// UseCaller connects reference validation and starts the queue worker.
func (s *Service) UseCaller(caller *catalog.Caller) {
	s.caller = caller
	s.workers.Every(100*time.Millisecond, s.tick)
}

// Stop waits for in-flight object operations to finish.
func (s *Service) Stop() { s.workers.Stop() }

type locationRecord struct {
	Efs          api.DescribeLocationEfsResponse
	Tags         map[string]string
	S3Config     *api.S3Config
	StorageClass *string
	ARN          string
	URI          string
	Kind         string
	Bucket       string
	Prefix       string
	Filesystem   string
	Version      int64
	Created      float64
}

type taskRecord struct {
	api.DescribeTaskResponse
	Tags     map[string]string
	Sequence int64
}

type executionRecord struct {
	api.DescribeTaskExecutionResponse
	Tags        map[string]string
	Cursor      *string
	Task        string
	Source      locationRecord
	Destination locationRecord
	Sequence    int64
	Revision    int64
	Deleting    bool
}

func invalid(format string, args ...any) error {
	return awserror.New("InvalidRequestException", fmt.Sprintf(format, args...))
}

func resourceARN(scope request.Scope, kind string) string {
	prefix := "loc-"
	if kind == kindTask {
		prefix = "task-"
	}

	return fmt.Sprintf(
		"arn:aws:datasync:%s:%s:%s/%s%s",
		scope.Region,
		scope.AccountID,
		kind,
		prefix,
		common.RandomID(17, "0123456789abcdef"),
	)
}

func recordKey(scope request.Scope, kind, arn string) (string, error) {
	prefix := fmt.Sprintf("arn:aws:datasync:%s:%s:%s/", scope.Region, scope.AccountID, kind)
	id, ok := strings.CutPrefix(arn, prefix)
	if !ok || id == "" {
		return "", invalid("Resource %s does not exist in this account and Region", arn)
	}

	return scope.Key(serviceName, kind, id), nil
}

func readRecord[T any](tx storage.ReadTx, scope request.Scope, kind, arn string) (T, error) {
	var zero T
	key, err := recordKey(scope, kind, arn)
	if err != nil {
		return zero, err
	}

	r, found, err := storage.Read[T](tx, key)
	if err != nil {
		return zero, err
	}

	if !found {
		return zero, invalid("Resource %s does not exist", arn)
	}

	return *r, nil
}

func writeRecord(tx storage.WriteTx, scope request.Scope, kind, arn string, record any) error {
	key, err := recordKey(scope, kind, arn)
	if err != nil {
		return err
	}

	return storage.Write(tx, key, record)
}

func pageLimit(n *int64) int {
	if n == nil || *n == 0 {
		return 100
	}

	return int(*n)
}

func timestamp() float64 { return float64(time.Now().UnixMilli()) / 1000 }

func (s *Service) requireRole(ctx context.Context, arn string) error {
	_, found, err := s.roles.RoleByARN(ctx, arn)
	if err != nil {
		return err
	}

	if !found {
		return invalid("IAM role %s does not exist", arn)
	}

	return nil
}

var _ api.API = (*Service)(nil)
