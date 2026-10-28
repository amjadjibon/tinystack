// Package batch stores Batch metadata and schedules local container jobs.
package batch

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "batch"
	kindCE         = "compute-environment"
	kindQueue      = "job-queue"
	kindDefinition = "job-definition"
	kindJob        = "job"
	enabled        = "ENABLED"
	disabled       = "DISABLED"
	managed        = "MANAGED"
	unmanaged      = "UNMANAGED"
	ec2Platform    = "EC2"
	active         = "ACTIVE"
	inactive       = "INACTIVE"
	valid          = "VALID"
	metadataReason = "Local compute environment metadata; no EC2 provisioning"
)

var _ api.API = (*Service)(nil)

// Network reads existing EC2 records using Batch's transaction and scope.
type Network interface {
	MountSubnet(storage.ReadTx, request.Scope, string) (*ec2api.Subnet, error)
	MountGroups(storage.ReadTx, request.Scope, string, []string) ([]string, error)
}

type Service struct {
	store     storage.Store
	network   Network
	runtime   ecs.Runtime
	writeLogs ecs.LogWriter
	bg        *background.Group
	busy      map[string]bool
	caller    *catalog.Caller
	owner     string
	lifecycle sync.RWMutex
	mu        sync.Mutex
}

func New(store storage.Store, network Network) *Service {
	s := &Service{store: store, network: network, bg: background.New(), busy: map[string]bool{}}

	return s
}

type record struct {
	Job                                                   *api.JobDetail
	CE                                                    *api.ComputeEnvironmentDetail
	Queue                                                 *api.JobQueueDetail
	Definition                                            *api.JobDefinition
	Tags                                                  map[string]string
	LogCursor                                             time.Time
	Scope                                                 request.Scope
	Parent, ContainerID, ComputeEnvironment, CancelReason string
	EventStatus                                           string
	ID, ARN, Kind                                         string
	Cancelled                                             bool
}

func invalid(message string) error { return awserror.New("ClientException", message) }
func missing() error               { return invalid("Resource does not exist in this account and Region") }

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func nameOK(value *string) error {
	if value == nil || !namePattern.MatchString(*value) {
		return invalid("Name requires 1-128 letters, numbers, hyphens or underscores")
	}

	return nil
}

func arn(scope request.Scope, kind, id string) string {
	return fmt.Sprintf("arn:aws:batch:%s:%s:%s/%s", scope.Region, scope.AccountID, kind, id)
}

func identifier(scope request.Scope, value *string, kind string) (string, error) {
	if value == nil {
		return "", invalid("Resource identifier is required")
	}

	id := *value
	if strings.HasPrefix(id, "arn:") {
		prefix := fmt.Sprintf("arn:aws:batch:%s:%s:%s/", scope.Region, scope.AccountID, kind)
		if !strings.HasPrefix(id, prefix) {
			return "", missing()
		}

		id = strings.TrimPrefix(id, prefix)
	}

	name := id
	if kind == kindDefinition {
		parts := strings.Split(id, ":")
		if len(parts) != 2 {
			return "", invalid("Job definition requires name:revision or a revisioned ARN")
		}

		n, err := strconv.ParseInt(parts[1], 10, 32)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != parts[1] {
			return "", invalid("Invalid job definition revision")
		}

		name = parts[0]
	}

	if kind == kindJob {
		return id, nil
	}

	if guardErr := nameOK(new(name)); guardErr != nil {
		return "", guardErr
	}

	return id, nil
}

func read(tx storage.ReadTx, scope request.Scope, kind string, value *string) (*record, error) {
	id, err := identifier(scope, value, kind)
	if err != nil {
		return nil, err
	}

	r, ok, err := storage.Read[record](tx, scope.Key(serviceName, kind, id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func rows(tx storage.ReadTx, scope request.Scope, kind string) ([]*record, error) {
	return storage.Records[record](tx, scope.Prefix(serviceName, kind))
}

func save(tx storage.WriteTx, scope request.Scope, r *record) error {
	if r.Job != nil {
		r.Job.Tags = maps.Clone(r.Tags)
	}

	return storage.Write(tx, scope.Key(serviceName, r.Kind, r.ID), r)
}

func unique(tx storage.WriteTx, scope request.Scope, r *record) error {
	_, ok, err := storage.Read[record](tx, scope.Key(serviceName, r.Kind, r.ID))
	if err != nil {
		return err
	}

	if ok {
		return invalid("Resource name already exists")
	}

	return save(tx, scope, r)
}

func ceOutput(r *record) api.ComputeEnvironmentDetail {
	out := *common.Clone(r.CE)
	out.Tags = maps.Clone(r.Tags)
	return out
}

func queueOutput(r *record) api.JobQueueDetail {
	out := *common.Clone(r.Queue)
	out.Tags = maps.Clone(r.Tags)
	return out
}

func definitionOutput(r *record) api.JobDefinition {
	out := *common.Clone(r.Definition)
	out.Tags = maps.Clone(r.Tags)
	return out
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(key, serviceName)
			if !ok {
				continue
			}

			r, _, e := storage.Read[record](tx, key)
			if e != nil {
				return e
			}

			details := map[string]any{"RecordsOnly": s.runtime == nil, "ExecutionReady": s.runtime != nil}
			if r.Job != nil {
				details["Job"] = common.Clone(r.Job)
			}

			if r.CE != nil {
				details["ComputeEnvironment"] = ceOutput(r)
			}

			if r.Queue != nil {
				details["JobQueue"] = queueOutput(r)
			}

			if r.Definition != nil {
				details["JobDefinition"] = definitionOutput(r)
			}

			out = append(
				out,
				catalog.Resource{
					Type:    r.Kind,
					Name:    r.ID,
					ARN:     r.ARN,
					Account: account,
					Region:  region,
					Tags:    r.Tags,
					Details: details,
				},
			)
		}

		return nil
	})
	return out, err
}
