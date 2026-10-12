// Package cloudformation implements AWS CloudFormation without HTTP
// dependencies. Stacks, their events, change sets, and exports belong to an
// account and region:
//
//	<scope>/cloudformation/stack/<name>                  (a stack, its resources, and its operation in progress)
//	<scope>/cloudformation/deleted/<stack UUID>          (deleted stacks, kept 90 days)
//	<scope>/cloudformation/event/<stack UUID>/<sequence> (each stack's events)
//	<scope>/cloudformation/changeset/<stack>/<name>
//	<scope>/cloudformation/export/<name>
//	aws/cloudformation/index/<account>/<region>/<stack>  (stacks with an operation in progress)
//
// Operations only validate a template and record what the stack must become.
// The runner (started by UseCaller, stopped by Stop) visits the index and
// moves each stack toward its template one resource at a time, outside
// transactions: each resource type (resources_*.go) creates, updates, and
// deletes its resource through the catalog.Caller with the operations of the
// service that owns it, and each step is recorded before the next, so a
// restarted TinyStack resumes where it stopped.
package cloudformation

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service       = "cloudformation"
	kindStack     = "stack"
	kindDeleted   = "deleted"
	kindEvent     = "event"
	kindChangeSet = "changeset"
	kindExport    = "export"
	indexPrefix   = "aws/" + service + "/index/"
	// stackType is the resource type of a stack's own events.
	stackType = "AWS::CloudFormation::Stack"
	// retention is how long deleted stacks stay listed.
	retention = 90 * 24 * time.Hour
)

var _ api.API = (*Service)(nil)

type Service struct {
	store    storage.Store
	caller   *catalog.Caller
	mq       mqRecords
	msk      mskRecords
	mwaa     mwaaRecords
	transfer transferRecords
	location locationRecords
	s3tables s3tablesRecords
	logger   *slog.Logger
	now      func() time.Time
	bg       *background.Group
	// wake starts a visit before the next tick, after an operation commits.
	wake chan struct{}
	// busy holds the stacks whose operations are advancing.
	busy map[string]bool
	mu   sync.Mutex
}

// New returns a CloudFormation service. Stacks change only once UseCaller
// starts the runner.
func New(store storage.Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store: store, logger: logger, now: time.Now, bg: background.New(),
		wake: make(chan struct{}, 1), busy: map[string]bool{},
	}
}

// UseCaller lets stacks manage their resources through the other services'
// operations and starts the runner. Call it before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) {
	s.caller = caller.Lenient()
	s.bg.Go(s.runner)
}

// Stop ends the runner and waits for the steps in progress.
func (s *Service) Stop() { s.bg.Stop() }

// poke wakes the runner after an operation commits.
func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// stack is a stack's record. Template and Parameters are what its resources
// must follow; during an update they are the new ones, and Op.Previous holds
// the ones a rollback returns to.
type stack struct {
	Created      time.Time
	Updated      *time.Time `json:",omitempty"`
	Deleted      *time.Time `json:",omitempty"`
	Op           *operation `json:",omitempty"`
	Resources    map[string]*resource
	ID           string
	Name         string
	Description  string `json:",omitempty"`
	Status       string
	Reason       string `json:",omitempty"`
	Template     string `json:",omitempty"`
	RoleARN      string `json:",omitempty"`
	ChangeSetID  string `json:",omitempty"`
	Token        string `json:",omitempty"`
	Endpoint     string `json:",omitempty"`
	Parameters   []parameter
	Tags         []tag
	Capabilities []string
	Outputs      []output
	// Replaced are the resources updates replaced, which the next cleanup
	// or the stack's deletion deletes.
	Replaced []*replaced `json:",omitempty"`
	// Imports are the exports the stack's template imports.
	Imports         []string
	Timeout         int64 `json:",omitempty"`
	Events          int64
	DisableRollback bool `json:",omitempty"`
	Protected       bool `json:",omitempty"`
}

// operation is a stack's create, update, or delete in progress.
type operation struct {
	Started time.Time
	// Previous is what an update's rollback returns to.
	Previous *revision `json:",omitempty"`
	// Action is create, update, or delete.
	Action string
	// Phase is apply (bring resources to the template), cleanup (delete
	// resources the template no longer has), or remove (delete them all).
	Phase     string
	OnFailure string `json:",omitempty"`
	Token     string `json:",omitempty"`
	// ChangeSet is the name of the change set being executed.
	ChangeSet string `json:",omitempty"`
	// Failed names the resource whose failure started a rollback.
	Failed   string   `json:",omitempty"`
	Retain   []string `json:",omitempty"`
	Rollback bool     `json:",omitempty"`
}

// revision is a stack's template and inputs before an update.
type revision struct {
	Template    string
	Description string `json:",omitempty"`
	Parameters  []parameter
	Tags        []tag
}

const (
	actionCreate = "CREATE"
	actionUpdate = "UPDATE"
	actionDelete = "DELETE"
	phaseApply   = "apply"
	phaseCleanup = "cleanup"
	phaseRemove  = "remove"
)

// resource is a stack resource: the properties it was last created or
// updated with, and what Ref and Fn::GetAtt return for it.
type resource struct {
	Updated    time.Time
	Properties map[string]any `json:",omitempty"`
	Attributes map[string]any `json:",omitempty"`
	Type       string
	Physical   string `json:",omitempty"`
	Status     string
	Reason     string `json:",omitempty"`
	Metadata   string `json:",omitempty"`
	// Policy is its DeletionPolicy.
	Policy string `json:",omitempty"`
	// Deps are the resources it depends on, which are deleted after it.
	Deps []string `json:",omitempty"`
}

// replaced is a resource an update replaced.
type replaced struct {
	Resource *resource
	Logical  string
}

type parameter struct {
	Key      string
	Value    string
	Resolved string `json:",omitempty"`
	NoEcho   bool   `json:",omitempty"`
}

type tag struct{ Key, Value string }

type output struct {
	Key         string
	Value       string
	Description string `json:",omitempty"`
	Export      string `json:",omitempty"`
}

// changeSet is a change set's record.
type changeSet struct {
	Created        time.Time
	ID             string
	Name           string
	StackID        string
	StackName      string
	Description    string `json:",omitempty"`
	Type           string
	Status         string
	Execution      string
	Reason         string `json:",omitempty"`
	Template       string
	RoleARN        string `json:",omitempty"`
	OnStackFailure string `json:",omitempty"`
	ClientToken    string `json:",omitempty"`
	Parameters     []parameter
	Tags           []tag
	Capabilities   []string
	Changes        []api.Change
}

// export is an output another stack can import.
type export struct {
	StackID   string
	StackName string
	Value     string
}

func indexKey(scope request.Scope, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

func eventKey(scope request.Scope, stackID string, n int64) string {
	return scope.Key(service, kindEvent, stackUUID(stackID)+"/"+fmt.Sprintf("%012d", n))
}

func stackARN(scope request.Scope, name string) string {
	return "arn:aws:cloudformation:" + scope.Region + ":" + scope.AccountID + ":stack/" + name + "/" +
		uuid.NewV4().String()
}

func changeSetARN(scope request.Scope, name string) string {
	return "arn:aws:cloudformation:" + scope.Region + ":" + scope.AccountID + ":changeSet/" + name + "/" +
		uuid.NewV4().String()
}

// stackUUID returns the last part of a stack ID.
func stackUUID(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '/' {
			return id[i+1:]
		}
	}

	return id
}

func validation(format string, args ...any) error {
	return awserror.New("ValidationError", fmt.Sprintf(format, args...))
}

func notExist(name string) error {
	return validation("Stack with id %s does not exist", name)
}
