package lambda

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// principalLambda is the service principal execution roles trust.
const principalLambda = "lambda.amazonaws.com"

// UseServiceAuthorizer makes the calls Lambda makes for a function, reading event sources and sending asynchronous
// results to destinations, require the function's execution role to trust lambda.amazonaws.com and to allow the call.
// Invoking a Lambda destination needs only the execution role's permission, as AWS documents. Call it before serving
// requests.
func (s *Service) UseServiceAuthorizer(authorizer catalog.ServiceAuthorizer) {
	s.authorizer = authorizer
}

// executionDelivery is the call as a delivery by the function with the given name, in scope, under its execution
// role. As the trust policy sees it, the function is the source resource.
func (s *Service) executionDelivery(ctx context.Context, scope request.Scope, name string, c *catalog.DeliveryRole) (
	catalog.ServiceDelivery, error,
) {
	var role string
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		fn, _, readErr := loadFunction(tx, scope.Key("lambda", "function", name))
		if fn != nil {
			role = fn.Role
		}

		return readErr
	})
	call := *c
	call.ARN, call.Session = role, name

	return catalog.ServiceDelivery{
		Principal:     principalLambda,
		SourceARN:     functionARN(scope, name),
		SourceAccount: scope.AccountID,
		Role:          &call,
	}, err
}

// authorizeReads checks that the function's execution role may read the event source, and returns the first call it
// may not make.
func (s *Service) authorizeReads(ctx context.Context, scope request.Scope, name, reader, source string) (
	denied catalog.DeliveryRole, err error,
) {
	d, err := s.executionDelivery(ctx, scope, name, &catalog.DeliveryRole{})
	if err != nil {
		return catalog.DeliveryRole{}, err
	}

	return s.checked.Authorize(ctx, s.authorizer, reader, source, d, s.now())
}

// readProblem is what a mapping reports as its last result when its execution role may not read the source.
// AWS does not document the exact wording for a denied read; keep this local diagnostic explicit.
func readProblem(denied catalog.DeliveryRole) string {
	label := map[string]string{
		serviceSQS: "SQS", "kinesis": "Kinesis", "dynamodbstreams": "DynamoDB Streams",
	}[denied.Service]

	return "PROBLEM: The provided execution role does not have permissions to call " + denied.Operation + " on " +
		label + "."
}

// checkMappingReads fails a mapping whose function's execution role may not read the event source, as AWS does when
// the mapping is created.
func (s *Service) checkMappingReads(ctx context.Context, scope request.Scope, mp *mapping) error {
	if mp.brokerSource() {
		return nil
	}

	defer s.checked.Delete(mp.UUID)
	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope})
	denied, err := s.authorizeReads(ctx, scope, mp.FunctionName, mp.UUID, mp.EventSourceArn)
	switch {
	case err == nil:
		return nil
	case denied.Service == serviceSQS:
		// https://repost.aws/questions/QUq_CtOWZGTH-ifO-mM1vXkQ
		return invalid("The provided execution role does not have permissions to call %s on SQS", denied.Operation)
	}

	// AWS reports the full diagnostic action list, including operations not used by TinyStack's poller.
	// https://repost.aws/questions/QUbrIKcZuoTF60lAlfrUGeng
	actions := "GetRecords, GetShardIterator, DescribeStream, and ListStreams"
	separator, suffix := ". ", " in IAM."
	if denied.Service == "kinesis" {
		// https://repost.aws/questions/QUR9HyviLyQDeuFR-whIKeMQ
		actions = "GetRecords, GetShardIterator, DescribeStream, DescribeStreamSummary, ListShards, and ListStreams"
		separator, suffix = ", ", "."
	}

	return invalid("Cannot access stream %s%sPlease ensure the role can perform the %s Actions on your stream%s",
		mp.EventSourceArn, separator, actions, suffix)
}
