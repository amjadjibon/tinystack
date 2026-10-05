// Package autoscaling implements EC2 Auto Scaling without HTTP dependencies.
// Groups and what they own belong to an account and region:
//
//	<scope>/autoscaling/group/<name>                     (settings, members, and tags)
//	<scope>/autoscaling/launch-configuration/<name>
//	<scope>/autoscaling/policy/<group>/<name>
//	<scope>/autoscaling/scheduled/<group>/<name>
//	<scope>/autoscaling/activity/<group>/<zero-padded ms><sequence>
//	aws/autoscaling/index/<account>/<region>/<group>     (every group, for the runner)
//
// Instances are EC2 instances, launched and terminated in-process through a
// catalog.Caller. The runner (started by New, stopped by Stop) visits every
// group each second: it applies due scheduled actions, replaces members EC2
// no longer reports running, and launches or terminates instances until the
// group has its desired capacity. Operations that change a group reconcile
// it before they return.
package autoscaling

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service            = "autoscaling"
	kindGroup          = "group"
	kindConfiguration  = "launch-configuration"
	kindPolicy         = "policy"
	kindScheduled      = "scheduled"
	kindActivity       = "activity"
	indexPrefix        = "aws/" + service + "/index/"
	defaultMaxRecords  = 50
	maxRecordsLimit    = 100
	maxNameLength      = 255
	resourceTypeGroup  = "auto-scaling-group"
	defaultCooldown    = 300
	healthHealthy      = "Healthy"
	healthUnhealthy    = "Unhealthy"
	lifecycleInService = "InService"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	logger *slog.Logger
	now    func() time.Time
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	stop   sync.Once
	// busy serializes reconciling, so a group is never scaled twice at once.
	busy sync.Mutex
	mu   sync.Mutex
	// seq orders activities that start in the same millisecond.
	seq uint64
}

// New returns an Auto Scaling service whose runner runs until Stop.
func New(store storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{store: store, logger: logger, now: time.Now, ctx: ctx, cancel: cancel}
	s.wg.Go(s.runner)
	return s
}

// UseCaller lets groups launch and terminate EC2 instances and target
// tracking policies manage their CloudWatch alarms. Call it before serving
// requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// Stop ends the runner and waits for it.
func (s *Service) Stop() {
	s.stop.Do(func() {
		s.cancel()
		s.wg.Wait()
	})
}

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

func indexKey(scope request.Scope, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

func arn(scope request.Scope, resource string) string {
	return "arn:aws:autoscaling:" + scope.Region + ":" + scope.AccountID + ":" + resource
}

func newID() string { return uuid.NewV4().String() }

// sequenced is a key suffix ordering records by time, then arrival.
func (s *Service) sequenced(t time.Time) string {
	s.mu.Lock()
	s.seq++
	n := s.seq
	s.mu.Unlock()
	return fmt.Sprintf("%015d%010d", t.UnixMilli(), n%1e10)
}

func validation(format string, args ...any) error {
	return awserror.New("ValidationError", fmt.Sprintf(format, args...))
}

func checkName(what, name string) error {
	if name == "" || len(name) > maxNameLength {
		return validation("%s must be 1 to 255 characters long.", what)
	}

	return nil
}

// window returns the page of items after the NextToken (the base64 key of
// the last item of the previous page), at most MaxRecords of them.
func window[T any](items []T, keyOf func(T) string, token *string, maxRecords *int64) (
	page []T, next *string, err error,
) {
	limit := int64(defaultMaxRecords)
	if maxRecords != nil {
		limit = *maxRecords
	}

	if limit < 1 || limit > maxRecordsLimit {
		return nil, nil, validation("MaxRecords must be 1 to 100.")
	}

	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil {
			return nil, nil, awserror.New("InvalidNextToken", "The token '"+*token+"' is invalid.")
		}

		for start < len(items) && keyOf(items[start]) <= string(after) {
			start++
		}
	}

	items = items[start:]
	if int64(len(items)) <= limit {
		return items, nil, nil
	}

	items = items[:limit]
	return items, new(base64.RawURLEncoding.EncodeToString([]byte(keyOf(items[len(items)-1])))), nil
}

// callContext is the context of an in-process call to another service.
func callContext(ctx context.Context, scope request.Scope, target string) context.Context {
	return request.WithMetadata(context.WithoutCancel(ctx),
		request.Metadata{Scope: scope, Service: target, RequestID: common.RandomID(32)})
}

// splitList splits a comma-separated list, dropping blanks.
func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}
