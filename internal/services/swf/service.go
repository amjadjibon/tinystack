// Package swf coordinates durable local deciders and activity workers.
package swf

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/swf"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName            = "swf"
	statusRegistered       = "REGISTERED"
	statusDeprecated       = "DEPRECATED"
	timeoutNone            = "NONE"
	timeoutStartToClose    = "START_TO_CLOSE"
	causeUnhandledDecision = "UNHANDLED_DECISION"
)

// Service implements Amazon SWF: domains, types, and workflow executions that deciders and activity workers advance
// by polling, with deadlines enforced by a background worker.
type Service struct {
	store   storage.Store
	workers *background.Group
	changed chan struct{}
	mu      sync.Mutex
}

// New returns a Service over store and starts its deadline worker.
func New(store storage.Store) *Service {
	s := &Service{store: store, workers: background.New(), changed: make(chan struct{})}
	s.workers.Every(100*time.Millisecond, s.tick)

	return s
}

// Stop stops the deadline worker and ends pending long polls.
func (s *Service) Stop() { s.workers.Stop() }

func (s *Service) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) watch() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

type domainRecord struct {
	Detail api.DomainDetail
	Tags   []api.ResourceTag
}

type typeRecord struct {
	Workflow *api.WorkflowTypeDetail
	Activity *api.ActivityTypeDetail
}

type activity struct {
	Attributes  api.ActivityTaskScheduledEventAttributes
	Details     *string
	Token       string
	Scheduled   int64
	Started     int64
	ScheduledAt float64
	StartedAt   float64
	HeartbeatAt float64
	CancelID    int64
}

type timer struct {
	ID      string
	Started int64
	Due     float64
}

type executionRecord struct {
	Scope             request.Scope
	Domain            string
	Key               string
	Detail            api.WorkflowExecutionDetail
	Activities        map[string]*activity
	Timers            map[string]*timer
	DecisionToken     string
	ParentKey         string
	Children          map[string]int64
	History           []api.HistoryEvent
	DecisionScheduled int64
	DecisionStarted   int64
	DecisionStartedAt float64
	PreviousStarted   int64
	ParentInitiated   int64
	ParentStarted     int64
	Deciding          bool `json:"-"`
	Dirty             bool
}

func fault(code, msg string) error { return awserror.New(code, msg) }
func invalid(msg string) error     { return fault("ValidationException", msg) }
func missing() error               { return fault("UnknownResourceFault", "Resource or task does not exist") }
func now() float64                 { return float64(time.Now().UnixNano()) / 1e9 }
func arn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:swf:%s:%s:/domain/%s", scope.Region, scope.AccountID, name)
}

func domainKey(scope request.Scope, name string) string {
	return scope.Key(serviceName, "domain", name)
}

func typeKey(scope request.Scope, domain, kind, name, version string) string {
	return scope.Key(serviceName, kind, domain+"/"+name+"/"+version)
}

func executionPrefix(scope request.Scope, domain string) string {
	return scope.Prefix(serviceName, "execution") + domain + "/"
}

// retentionPrefix holds closed runs by expiry time; indexPrefix holds the open runs the worker visits.
const (
	indexPrefix     = "aws/swf/index/"
	retentionPrefix = "aws/swf/retention/"
)

func indexKey(r *executionRecord) string { return indexPrefix + r.Key }
func openKey(scope request.Scope, domain, workflow string) string {
	return scope.Key(serviceName, "open", domain+"/"+workflow)
}

func readDomain(tx storage.ReadTx, scope request.Scope, name string, registered bool) (*domainRecord, error) {
	r, ok, err := storage.Read[domainRecord](tx, domainKey(scope, name))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	if registered && common.Value(r.Detail.DomainInfo.Status) != statusRegistered {
		return nil, fault("DomainDeprecatedFault", "Domain is deprecated")
	}

	return r, nil
}

func readType(tx storage.ReadTx, scope request.Scope, domain, kind, name, version string) (*typeRecord, error) {
	r, ok, err := storage.Read[typeRecord](tx, typeKey(scope, domain, kind, name, version))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, missing()
	}

	return r, nil
}

func readExecution(tx storage.ReadTx, scope request.Scope, domain, workflow, run string) (*executionRecord, error) {
	if _, err := readDomain(tx, scope, domain, false); err != nil {
		return nil, err
	}

	if run == "" {
		key, ok, err := storage.Read[string](tx, openKey(scope, domain, workflow))
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, missing()
		}

		r, ok, err := storage.Read[executionRecord](tx, *key)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, missing()
		}

		return r, nil
	}

	records, err := storage.Records[executionRecord](tx, executionPrefix(scope, domain))
	if err != nil {
		return nil, err
	}

	for _, r := range records {
		if common.Value(r.Detail.ExecutionInfo.Execution.WorkflowId) == workflow &&
			common.Value(r.Detail.ExecutionInfo.Execution.RunId) == run {
			return r, nil
		}
	}

	return nil, missing()
}

func (s *Service) update(ctx context.Context, fn func(storage.WriteTx, request.Scope) error) error {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error { return fn(tx, scope) })
	if err == nil {
		s.notify()
	}

	return err
}

func isOpen(r *executionRecord) bool {
	return common.Value(r.Detail.ExecutionInfo.ExecutionStatus) == "OPEN"
}

func requireOpen(r *executionRecord) error {
	if !isOpen(r) {
		return missing()
	}

	return nil
}

// Timeouts and priorities are strings in Smithy, so their semantic ranges need explicit validation.
func timeout(value *string, none bool) error {
	if value == nil {
		return nil
	}

	if *value == timeoutNone && none {
		return nil
	}

	n, err := strconv.ParseInt(*value, 10, 64)
	if err != nil || n < 0 || n > 31536000 {
		return invalid("Timeout must be seconds in 0..31536000 or NONE where supported")
	}

	return nil
}

func priority(value *string) error {
	if value == nil {
		return nil
	}

	if _, err := strconv.ParseInt(*value, 10, 32); err != nil {
		return invalid("Priority must be a signed 32-bit integer")
	}

	return nil
}

func deadline(start float64, value *string) float64 {
	if value == nil || *value == timeoutNone {
		return 0
	}

	n, err := strconv.ParseInt(*value, 10, 64)
	if err != nil {
		return 0
	}

	return start + float64(n)
}

func expired(start float64, value *string, t float64) bool {
	d := deadline(start, value)
	return d != 0 && t >= d
}

func limit(n *int64) int {
	if n == nil || *n == 0 {
		return 1000
	}

	return int(*n)
}

var _ api.API = (*Service)(nil)
