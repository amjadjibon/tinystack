// Package pipes implements Amazon EventBridge Pipes without HTTP
// dependencies. A pipe reads records from a source, filters them, optionally
// enriches them with a Lambda function, and delivers them to a target.
// Pipes belong to an account and region:
//
//	<scope>/pipes/pipe/<name>
//	aws/pipes/index/<account>/<region>/<name>  (each pipe whose desired state is RUNNING)
//
// The runner (started by New, stopped by Stop) visits the index every second
// and moves batches of each running pipe: SQS queues are read in-process and
// their messages deleted once delivered or filtered out, and Kinesis and
// DynamoDB streams are read through kinesis.Service and dynamodb.Streams
// with cursors kept in the pipe's record. Enrichment functions and targets
// (Lambda, SQS, SNS, EventBridge, Step Functions, and Kinesis) are called
// in-process through a catalog.Caller.
package pipes

import (
	"log/slog"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/pipes"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/background"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service     = "pipes"
	kindPipe    = "pipe"
	indexPrefix = "aws/" + service + "/index/"

	stateRunning  = "RUNNING"
	stateStopped  = "STOPPED"
	stateDeleted  = "DELETED"
	stateCreating = "CREATING"
	stateStarting = "STARTING"
	stateStopping = "STOPPING"
	stateUpdating = "UPDATING"
	stateDeleting = "DELETING"

	maxName        = 64
	maxDescription = 512
	maxTemplate    = 8192
	maxFilters     = 5
	defaultLimit   = 100
	maxBatchSize   = 10000
	defaultBatch   = 10
	streamBatch    = 100
	maxSQSBatch    = 10
)

var _ api.API = (*Service)(nil)

// Service is the EventBridge Pipes service.
type Service struct {
	store   storage.Store
	queues  *sqs.Service
	kinesis *kinesis.Service
	streams *dynamodb.Streams
	caller  *catalog.Caller
	// authorizer checks that the pipe's role may be assumed and may make each call; see UseServiceAuthorizer.
	authorizer catalog.ServiceAuthorizer
	logger     *slog.Logger
	now        func() time.Time
	bg         *background.Group
}

// New returns a Pipes service whose runner moves records of running pipes
// until Stop.
func New(
	store storage.Store,
	queues *sqs.Service,
	streams *kinesis.Service,
	tables *dynamodb.Streams,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{
		store: store, queues: queues, kinesis: streams, streams: tables, logger: logger, now: time.Now,
		bg: background.New(),
	}
	s.bg.Every(runInterval, s.visit)

	return s
}

// UseCaller lets pipes invoke enrichment functions and targets. Call it
// before serving requests.
func (s *Service) UseCaller(caller *catalog.Caller) { s.caller = caller }

// UseServiceAuthorizer makes every enrichment and target call require the pipe's role to trust
// pipes.amazonaws.com and to allow the call. Call it before serving requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// Stop ends the runner and waits for it.
func (s *Service) Stop() { s.bg.Stop() }

// pipe is a pipe's record. Cursor (a DynamoDB stream's last sequence number)
// and Cursors (a Kinesis stream's, by shard, or "done" for a closed shard
// read to its end) are where the runner resumes.
type pipe struct {
	SourceParameters     *api.PipeSourceParameters
	EnrichmentParameters *api.PipeEnrichmentParameters
	TargetParameters     *api.PipeTargetParameters
	Tags                 map[string]string
	Cursors              map[string]string `json:",omitempty"`
	Name                 string
	Description          string
	DesiredState         string
	Source               string
	Enrichment           string
	Target               string
	RoleArn              string
	Cursor               string `json:",omitempty"`
	Created              float64
	Modified             float64
}

func indexKey(scope request.Scope, name string) string {
	return indexPrefix + scope.AccountID + "/" + scope.Region + "/" + name
}

func pipeARN(scope request.Scope, name string) string {
	return "arn:aws:pipes:" + scope.Region + ":" + scope.AccountID + ":pipe/" + name
}

// currentState is the state a pipe settles in.
func (p *pipe) currentState() string {
	if p.DesiredState == stateRunning {
		return stateRunning
	}

	return stateStopped
}

func notFound(name string) error {
	return awserror.New("NotFoundException", "Pipe "+name+" does not exist.")
}
