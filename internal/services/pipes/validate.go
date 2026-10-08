package pipes

import (
	"context"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/pipes"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/pattern"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
)

const (
	positionTrim   = "TRIM_HORIZON"
	positionLatest = "LATEST"

	invokeSync  = "REQUEST_RESPONSE"
	invokeAsync = "FIRE_AND_FORGET"

	sourceSQS      = "sqs"
	sourceKinesis  = "kinesis"
	sourceDynamoDB = "dynamodb"

	targetLambda      = "lambda"
	targetSQS         = "sqs"
	targetSNS         = "sns"
	targetEventBridge = "events"
	targetStates      = "states"
	targetKinesis     = "kinesis"
)

var (
	namePattern = regexp.MustCompile(`^[.\-_A-Za-z0-9]+$`)
	rolePattern = regexp.MustCompile(`^arn:(aws[a-zA-Z-]*)?:iam::\d{12}:role/?[a-zA-Z0-9+=,.@\-_/]+$`)

	// filterPatterns is the event filtering syntax, which has no wildcard.
	filterPatterns = pattern.Dialect{MaxNumeric: 5e9}
)

// arnService returns the service an ARN names and its account and region.
func arnService(arn string) (svc string, scope request.Scope, resource string, ok bool) {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" {
		return "", request.Scope{}, "", false
	}

	return parts[2], request.Scope{AccountID: parts[4], Region: parts[3]}, parts[5], true
}

// sourceKind classifies a source ARN as an SQS queue, a Kinesis stream, or a
// DynamoDB stream.
func sourceKind(arn string) (string, bool) {
	svc, _, _, ok := arnService(arn)
	if !ok {
		return "", false
	}

	switch svc {
	case sourceSQS:
		_, queue := sqs.QueueURL(arn)
		return sourceSQS, queue
	case sourceKinesis:
		_, _, stream := kinesis.ParseStreamARN(arn)
		return sourceKinesis, stream
	case sourceDynamoDB:
		_, _, _, stream := dynamodb.ParseStreamARN(arn)
		return sourceDynamoDB, stream
	}

	return "", false
}

// targetKind classifies a target ARN by the kind of resource it names.
func targetKind(arn string) (string, bool) {
	svc, _, resource, ok := arnService(arn)
	if !ok {
		return "", false
	}

	switch svc {
	case targetLambda:
		return svc, strings.HasPrefix(resource, "function:")
	case targetSQS, targetSNS:
		return svc, resource != ""
	case targetEventBridge:
		return svc, strings.HasPrefix(resource, "event-bus/")
	case targetStates:
		return svc, strings.HasPrefix(resource, "stateMachine:")
	case targetKinesis:
		return svc, strings.HasPrefix(resource, "stream/")
	}

	return "", false
}

// check validates a pipe's configuration as a whole. It reads the source, so
// it runs outside transactions. A new pipe starting at LATEST has its cursors
// set where its stream ends now.
func (s *Service) check(ctx context.Context, scope request.Scope, p *pipe, created bool) error {
	switch {
	case len(p.Name) > maxName || !namePattern.MatchString(p.Name):
		return awserror.Validation("Name must be 1 to %d characters of letters, numbers, and .-_", maxName)
	case len(p.Description) > maxDescription:
		return awserror.Validation("Description must be at most %d characters long", maxDescription)
	case p.DesiredState != stateRunning && p.DesiredState != stateStopped:
		return awserror.Validation("DesiredState must be RUNNING or STOPPED")
	case !rolePattern.MatchString(p.RoleArn):
		return awserror.Validation("RoleArn must be the ARN of an IAM role")
	}

	source, ok := sourceKind(p.Source)
	if !ok {
		return awserror.Validation(
			"TinyStack supports SQS queues, Kinesis streams, and DynamoDB streams as pipe sources, "+
				"not %q",
			p.Source,
		)
	}

	if err := s.checkSource(ctx, scope, p, source, created); err != nil {
		return err
	}

	if err := s.checkEnrichment(scope, p); err != nil {
		return err
	}

	return checkTarget(scope, p)
}

// inScope checks that an ARN names a resource of the pipe's account and
// region.
func inScope(what, arn string, scope request.Scope) error {
	if _, got, _, _ := arnService(arn); got != scope {
		return awserror.Validation("The %s must be in the pipe's account and region", what)
	}

	return nil
}

func (s *Service) checkSource(ctx context.Context, scope request.Scope, p *pipe, kind string, created bool) error {
	if err := inScope("source", p.Source, scope); err != nil {
		return err
	}

	if p.SourceParameters == nil {
		p.SourceParameters = &api.PipeSourceParameters{}
	}

	sp := p.SourceParameters
	wrong := map[string]bool{
		"SqsQueueParameters":       sp.SqsQueueParameters != nil && kind != sourceSQS,
		"KinesisStreamParameters":  sp.KinesisStreamParameters != nil && kind != sourceKinesis,
		"DynamoDBStreamParameters": sp.DynamoDBStreamParameters != nil && kind != sourceDynamoDB,
	}
	for name, bad := range wrong {
		if bad {
			return awserror.Validation("SourceParameters.%s does not apply to the pipe's source", name)
		}
	}

	if err := checkFilters(sp.FilterCriteria); err != nil {
		return err
	}

	switch kind {
	case sourceSQS:
		return s.checkQueue(ctx, p)
	case sourceKinesis:
		return s.checkKinesis(ctx, p, created)
	}

	return s.checkStream(ctx, p, created)
}

func checkFilters(criteria *api.FilterCriteria) error {
	if criteria == nil {
		return nil
	}

	if len(criteria.Filters) > maxFilters {
		return awserror.Validation("FilterCriteria can have at most %d filters", maxFilters)
	}

	for _, f := range criteria.Filters {
		if _, err := pattern.Compile(common.Value(f.Pattern), filterPatterns); err != nil {
			return awserror.Validation("Invalid filter pattern definition. %s", err.Error())
		}
	}

	return nil
}

// checkBatch fills a source's batch size and checks it and the batching
// window, which TinyStack does not hold records for.
func checkBatch(size, window **int64, fallback, limit int64) error {
	if *size == nil {
		*size = new(fallback)
	}

	if **size < 1 || **size > limit {
		return awserror.Validation("BatchSize must be between 1 and %d for this source", limit)
	}

	if *window == nil {
		*window = new(int64(0))
	}

	if **window != 0 {
		return awserror.Validation("TinyStack does not support MaximumBatchingWindowInSeconds")
	}

	return nil
}

func (s *Service) checkQueue(ctx context.Context, p *pipe) error {
	q := p.SourceParameters.SqsQueueParameters
	if q == nil {
		q = &api.PipeSourceSqsQueueParameters{}
		p.SourceParameters.SqsQueueParameters = q
	}

	if batchErr := checkBatch(
		&q.BatchSize,
		&q.MaximumBatchingWindowInSeconds,
		defaultBatch,
		maxSQSBatch,
	); batchErr != nil {
		return batchErr
	}

	name := p.Source[strings.LastIndex(p.Source, ":")+1:]
	if _, err := s.queues.GetQueueUrl(ctx, &sqsapi.GetQueueUrlRequest{QueueName: &name}); err != nil {
		return awserror.Validation("The source queue %s does not exist", p.Source)
	}

	return nil
}

// startingPosition checks a stream source's position and returns whether it
// is LATEST.
func startingPosition(position *string) (latest bool, err error) {
	switch common.Value(position) {
	case positionTrim:
		return false, nil
	case positionLatest:
		return true, nil
	case "":
		return false, awserror.Validation("StartingPosition is required for stream sources")
	}

	return false, awserror.Validation("TinyStack supports the TRIM_HORIZON and LATEST starting positions")
}

func (s *Service) checkKinesis(ctx context.Context, p *pipe, created bool) error {
	k := p.SourceParameters.KinesisStreamParameters
	if k == nil {
		return awserror.Validation("SourceParameters.KinesisStreamParameters with a StartingPosition is required")
	}

	if k.StartingPositionTimestamp != nil {
		return awserror.Validation("TinyStack does not support StartingPositionTimestamp")
	}

	latest, err := startingPosition(k.StartingPosition)
	if err != nil {
		return err
	}

	if batchErr := checkBatch(
		&k.BatchSize,
		&k.MaximumBatchingWindowInSeconds,
		streamBatch,
		maxBatchSize,
	); batchErr != nil {
		return batchErr
	}

	shards, err := s.kinesis.Shards(ctx, p.Source)
	if err != nil {
		return awserror.Validation("The source stream %s does not exist", p.Source)
	}

	if created {
		p.Cursors = map[string]string{}
		for _, sh := range shards {
			if latest {
				p.Cursors[sh.ID] = sh.Tip
			}
		}
	}

	return nil
}

func (s *Service) checkStream(ctx context.Context, p *pipe, created bool) error {
	d := p.SourceParameters.DynamoDBStreamParameters
	if d == nil {
		return awserror.Validation("SourceParameters.DynamoDBStreamParameters with a StartingPosition is required")
	}

	latest, err := startingPosition(d.StartingPosition)
	if err != nil {
		return err
	}

	if batchErr := checkBatch(
		&d.BatchSize,
		&d.MaximumBatchingWindowInSeconds,
		streamBatch,
		maxBatchSize,
	); batchErr != nil {
		return batchErr
	}

	last, err := s.streams.Latest(ctx, p.Source)
	if err != nil {
		return awserror.Validation("The source stream %s does not exist", p.Source)
	}

	if created && latest {
		p.Cursor = last
	}

	return nil
}

func (s *Service) checkEnrichment(scope request.Scope, p *pipe) error {
	if p.Enrichment == "" {
		if p.EnrichmentParameters != nil && p.EnrichmentParameters.InputTemplate != nil {
			return awserror.Validation("EnrichmentParameters apply only to a pipe with an Enrichment")
		}

		return nil
	}

	if kind, ok := targetKind(p.Enrichment); !ok || kind != targetLambda {
		return awserror.Validation("TinyStack supports only Lambda functions as pipe enrichments, not %q", p.Enrichment)
	}

	if err := inScope("enrichment", p.Enrichment, scope); err != nil {
		return err
	}

	if p.EnrichmentParameters != nil && p.EnrichmentParameters.InputTemplate != nil {
		return checkTemplate("EnrichmentParameters.InputTemplate", *p.EnrichmentParameters.InputTemplate)
	}

	return nil
}

func checkTarget(scope request.Scope, p *pipe) error {
	kind, ok := targetKind(p.Target)
	if !ok {
		return awserror.Validation(
			"TinyStack supports Lambda functions, SQS queues, SNS topics, EventBridge event buses, "+
				"Step Functions state machines, and Kinesis streams as pipe targets, not %q",
			p.Target,
		)
	}

	if err := inScope("target", p.Target, scope); err != nil {
		return err
	}

	if p.TargetParameters == nil {
		p.TargetParameters = &api.PipeTargetParameters{}
	}

	tp := p.TargetParameters
	wrong := map[string]bool{
		"LambdaFunctionParameters":           tp.LambdaFunctionParameters != nil && kind != targetLambda,
		"StepFunctionStateMachineParameters": tp.StepFunctionStateMachineParameters != nil && kind != targetStates,
		"KinesisStreamParameters":            tp.KinesisStreamParameters != nil && kind != targetKinesis,
		"SqsQueueParameters":                 tp.SqsQueueParameters != nil && kind != targetSQS,
		"EventBridgeEventBusParameters":      tp.EventBridgeEventBusParameters != nil && kind != targetEventBridge,
	}
	for name, bad := range wrong {
		if bad {
			return awserror.Validation("TargetParameters.%s does not apply to the pipe's target", name)
		}
	}

	if tp.InputTemplate != nil {
		if err := checkTemplate("TargetParameters.InputTemplate", *tp.InputTemplate); err != nil {
			return err
		}
	}

	return checkTargetParameters(kind, tp)
}

func checkTargetParameters(kind string, tp *api.PipeTargetParameters) error {
	switch kind {
	case targetLambda:
		if l := tp.LambdaFunctionParameters; l != nil {
			if t := common.Value(l.InvocationType); t != "" && t != invokeSync && t != invokeAsync {
				return awserror.Validation("InvocationType must be REQUEST_RESPONSE or FIRE_AND_FORGET")
			}
		}
	case targetStates:
		if m := tp.StepFunctionStateMachineParameters; m != nil {
			if t := common.Value(m.InvocationType); t != "" && t != invokeAsync {
				return awserror.Validation(
					"TinyStack supports only the FIRE_AND_FORGET invocation type for state machines",
				)
			}
		}
	case targetKinesis:
		if tp.KinesisStreamParameters == nil || common.Value(tp.KinesisStreamParameters.PartitionKey) == "" {
			return awserror.Validation(
				"TargetParameters.KinesisStreamParameters.PartitionKey is required for Kinesis targets",
			)
		}
	case targetEventBridge:
		if e := tp.EventBridgeEventBusParameters; e != nil {
			if strings.HasPrefix(common.Value(e.Source), "$") || strings.HasPrefix(common.Value(e.DetailType), "$") {
				return awserror.Validation("TinyStack does not support JSON paths in EventBridgeEventBusParameters")
			}
		}
	}

	return nil
}
