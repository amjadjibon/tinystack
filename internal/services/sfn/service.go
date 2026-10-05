// Package sfn implements AWS Step Functions without HTTP dependencies: state
// machines, activities, and executions that an Amazon States Language
// interpreter (JSONPath) runs in the background. Task states call Lambda
// functions, activities, the optimized service integrations, and AWS SDK
// integrations in-process through a catalog.Caller. Resources belong to an
// account and region:
//
//	<scope>/sfn/statemachine/<name>
//	<scope>/sfn/activity/<name>
//	<scope>/sfn/execution/<state machine>/<execution>
//	<scope>/sfn/event/<state machine>/<execution>/<zero-padded event ID>
//
// Task tokens, activity task queues, and running executions live in memory:
// executions running when TinyStack stops are aborted when it starts again.
package sfn

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	errRuntime     = "States.Runtime"
	errTimeout     = "States.Timeout"
	errTaskFailed  = "States.TaskFailed"
	errHeartbeat   = "States.HeartbeatTimeout"
	stateTask      = "Task"
	statePass      = "Pass"
	stateChoice    = "Choice"
	stateWait      = "Wait"
	stateSucceed   = "Succeed"
	stateFail      = "Fail"
	stateParallel  = "Parallel"
	stateMap       = "Map"
	eventActivity  = "Activity"
	keyName        = "Name"
	svcDynamoDB    = "dynamodb"
	svcEventBridge = "eventbridge"
	// states is the ARN service name of Step Functions, and its integrations'.
	states = "states"
	// arnStateMachine is the resource type of state machine ARNs.
	arnStateMachine = "stateMachine"
)

const (
	service         = "sfn"
	kindMachine     = "statemachine"
	kindActivity    = "activity"
	kindExecution   = "execution"
	kindEvent       = "event"
	typeStandard    = "STANDARD"
	typeExpress     = "EXPRESS"
	statusRunning   = "RUNNING"
	statusSucceeded = "SUCCEEDED"
	statusFailed    = "FAILED"
	statusAborted   = "ABORTED"
	statusTimedOut  = "TIMED_OUT"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	logs   *logs.Service
	caller *catalog.Caller
	logger *slog.Logger
	now    func() time.Time
	// bg runs executions until Stop.
	bg *background.Group
	// running cancels a running execution by ARN, for StopExecution.
	running map[string]context.CancelCauseFunc
	// tokens are the task tokens states wait on.
	tokens map[string]*waiter
	// queues hold the tasks scheduled on each activity, by ARN.
	queues map[string]chan *activityTask
	mu     sync.Mutex
}

// New returns a Step Functions service that writes execution logs through
// logGroups. Executions left running by an earlier process are aborted.
func New(store storage.Store, logGroups *logs.Service, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{
		store: store, logs: logGroups, logger: logger, now: time.Now, bg: background.New(),
		running: map[string]context.CancelCauseFunc{}, tokens: map[string]*waiter{},
		queues: map[string]chan *activityTask{},
	}

	err := s.abortOrphans()
	return s, err
}

// UseCaller lets Task states call the other services. Step Functions is one
// of the services the caller holds, so it is registered after New; call it
// before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// Stop ends running executions and waits for them.
func (s *Service) Stop() { s.bg.Stop() }

// abortOrphans aborts the executions an earlier process left running.
func (s *Service) abortOrphans() error {
	return s.store.Update(context.Background(), func(tx storage.WriteTx) error {
		for _, key := range tx.Keys("") {
			if _, _, rest, ok := storage.Scoped(key, service); !ok || !strings.HasPrefix(rest, kindExecution+"/") {
				continue
			}

			e, _, err := storage.Read[execution](tx, key)
			if err != nil || e.Status != statusRunning {
				if err != nil {
					return err
				}

				continue
			}

			e.Status, e.Stop = statusAborted, common.Unix(s.now())
			e.Error, e.Cause = errRuntime, "TinyStack stopped while the execution was running"
			if writeErr := storage.Write(tx, key, e); writeErr != nil {
				return writeErr
			}
		}

		return nil
	})
}

// stateMachine is a state machine's record.
type stateMachine struct {
	Tags       map[string]string
	Logging    *api.LoggingConfiguration
	Tracing    *api.TracingConfiguration
	Name       string
	Definition string
	RoleArn    string
	Type       string
	RevisionID string
	Created    float64
	// LastVersion is the number of the last version published, so numbers are
	// never reused.
	LastVersion int64 `json:",omitempty"`
}

// execution is an execution's record; its history events are records of
// their own.
//
// Definition is the definition the execution runs, which a redrive runs
// again from Resume, where it stopped; VersionARN and AliasARN name the
// version that ran and the alias that chose it, if it started from one; and
// Redrives counts redrives, with RedriveDate and RedriveToken the last's.
type execution struct {
	Resume          *resumePoint `json:",omitempty"`
	RedriveToken    string       `json:",omitempty"`
	Status          string
	StateMachineARN string
	Name            string
	AliasARN        string `json:",omitempty"`
	Definition      string `json:",omitempty"`
	ARN             string
	Endpoint        string
	StateMachine    string
	Cause           string `json:",omitempty"`
	VersionARN      string `json:",omitempty"`
	Input           string
	Output          string  `json:",omitempty"`
	Error           string  `json:",omitempty"`
	RedriveDate     float64 `json:",omitempty"`
	Redrives        int64   `json:",omitempty"`
	Start           float64
	Stop            float64 `json:",omitempty"`
	Events          int64
}

// activity is an activity's record.
type activity struct {
	Tags    map[string]string
	Name    string
	Created float64
}

func machineKey(scope request.Scope, name string) string {
	return scope.Prefix(service, kindMachine) + name
}

func activityKey(scope request.Scope, name string) string {
	return scope.Prefix(service, kindActivity) + name
}

func executionKey(scope request.Scope, machine, name string) string {
	return scope.Prefix(service, kindExecution) + machine + "/" + name
}

func eventKey(scope request.Scope, machine, name string, id int64) string {
	return fmt.Sprintf("%s%s/%s/%010d", scope.Prefix(service, kindEvent), machine, name, id)
}

func arn(scope request.Scope, kind, name string) string {
	return "arn:aws:states:" + scope.Region + ":" + scope.AccountID + ":" + kind + ":" + name
}

// parseARN returns the scope, kind, and name of a Step Functions ARN, whose
// name is everything after the kind.
func parseARN(s string) (scope request.Scope, kind, name string, ok bool) {
	parts := strings.SplitN(s, ":", 7)
	if len(parts) != 7 || parts[0] != "arn" || parts[2] != states {
		return request.Scope{}, "", "", false
	}

	return request.Scope{AccountID: parts[4], Region: parts[3]}, parts[5], parts[6], true
}

func newToken() string {
	var b [96]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

var namePattern = regexp.MustCompile("^[^ <>{}\\[\\]?*\"#%\\\\^|~`$&,;:/\\x00-\\x1f\\x7f-\\x9f]{1,80}$")

func checkName(name string) error {
	if !namePattern.MatchString(name) {
		return awserror.New("InvalidName", "Invalid Name: '"+name+"'")
	}

	return nil
}

func invalidARN(s string) error { return awserror.New("InvalidArn", "Invalid Arn: '"+s+"'") }

// stateError is an error states raise and Retry and Catch match by name.
type stateError struct {
	Name  string
	Cause string
}

func (e *stateError) Error() string { return e.Name + ": " + e.Cause }

// asStateError names an error: a stateError as it is, a canceled context as
// the reason it was canceled, and anything else as States.Runtime.
func asStateError(err error) *stateError {
	var se *stateError
	if errors.As(err, &se) {
		return se
	}

	return &stateError{Name: errRuntime, Cause: err.Error()}
}

func tagsOf(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[common.Value(t.Key)] = common.Value(t.Value)
	}

	return out
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })
}

// page returns the items after the one token names, at most limit of them
// (1 to 1000, 100 by default), and the token for the rest, if any.
func page[T any](items []T, id func(T) string, token *string, limit *int64) (selected []T, next *string, err error) {
	n := int64(100)
	if limit != nil && *limit != 0 {
		if *limit < 0 || *limit > 1000 {
			return nil, nil, awserror.Validation("maxResults must be between 0 and 1000")
		}

		n = *limit
	}

	return common.Page(
		items,
		id,
		token,
		int(n),
		awserror.New("InvalidToken", "Invalid Token: '"+common.Value(token)+"'"),
	)
}

// optional returns s, or nil when it is empty.
func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
