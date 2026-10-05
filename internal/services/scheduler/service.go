// Package scheduler implements EventBridge Scheduler without HTTP
// dependencies. Schedules and schedule groups belong to an account and
// region:
//
//	<scope>/scheduler/group/<name>                       (the default group is implicit)
//	<scope>/scheduler/schedule/<group>/<name>
//	aws/scheduler/index/<account>/<region>/<group>/<name> (the next run of each schedule that has one)
//
// The runner (started by New, stopped by Stop) reads the index every second,
// claims each due run by advancing the schedule's next run in a transaction,
// and then invokes the target in-process through a catalog.Caller: Lambda,
// SQS, SNS, EventBridge, Step Functions, and Kinesis templated targets, and
// universal targets (arn:aws:scheduler:::aws-sdk:<service>:<action>) for any
// operation TinyStack implements. A failed invocation goes to the target's
// SQS dead-letter queue.
package scheduler

import (
	"log/slog"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service       = "scheduler"
	kindGroup     = "group"
	kindSchedule  = "schedule"
	indexPrefix   = "aws/" + service + "/index/"
	defaultGroup  = "default"
	stateEnabled  = "ENABLED"
	stateDisabled = "DISABLED"
	groupActive   = "ACTIVE"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	logger *slog.Logger
	now    func() time.Time
	bg     *background.Group
}

// New returns a Scheduler service whose runner runs until Stop.
func New(store storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{store: store, logger: logger, now: time.Now, bg: background.New()}
	s.bg.Every(runInterval, s.visit)
	return s
}

// UseCaller lets schedules invoke their targets. Call it before serving
// requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// Stop ends the runner and waits for it.
func (s *Service) Stop() { s.bg.Stop() }

// group is a schedule group's record.
type group struct {
	Tags     map[string]string
	Name     string
	Created  float64
	Modified float64
}

// record is a schedule's record. Next is its next run in Unix seconds, or
// zero when it has none.
type record struct {
	Schedule api.GetScheduleOutput
	Next     float64 `json:",omitempty"`
}

func key(scope request.Scope, kind, name string) string {
	return scope.AccountID + "/" + scope.Region + "/" + service + "/" + kind + "/" + name
}

func indexKey(scope request.Scope, groupName, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + groupName + "/" + name
}

func groupARN(scope request.Scope, name string) string {
	return "arn:aws:scheduler:" + scope.Region + ":" + scope.AccountID + ":schedule-group/" + name
}

func scheduleARN(scope request.Scope, groupName, name string) string {
	return "arn:aws:scheduler:" + scope.Region + ":" + scope.AccountID + ":schedule/" + groupName + "/" + name
}

func fromUnix(f float64) time.Time { return time.UnixMilli(int64(f * 1000)).UTC() }

func validation(format string, args ...any) error {
	return awserror.Validation(format, args...)
}

func notFound(what string) error {
	return awserror.New("ResourceNotFoundException", what+" does not exist.")
}

// loadGroup reads a schedule group; the default group always exists.
func loadGroup(tx storage.ReadTx, scope request.Scope, name string) (*group, error) {
	if name == defaultGroup {
		return &group{Name: defaultGroup, Tags: map[string]string{}}, nil
	}

	g, found, err := storage.Read[group](tx, key(scope, kindGroup, name))
	if err == nil && !found {
		err = notFound("Schedule group " + name)
	}

	return g, err
}
