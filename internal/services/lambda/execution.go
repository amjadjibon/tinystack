package lambda

import (
	"context"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/eventsource"
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
func (s *Service) authorizeReads(ctx context.Context, scope request.Scope, name, source string) (
	denied catalog.DeliveryRole, err error,
) {
	reads := eventsource.Reads(source)
	for i := range reads {
		d, loadErr := s.executionDelivery(ctx, scope, name, &reads[i])
		if loadErr != nil {
			return reads[i], loadErr
		}

		if err := s.authorizer.Authorize(ctx, d); err != nil {
			return reads[i], err
		}
	}

	return catalog.DeliveryRole{}, nil
}

// readProblem is what a mapping reports as its last result when its execution role may not read the source.
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
	ctx = request.WithMetadata(context.WithoutCancel(ctx), request.Metadata{Scope: scope})
	denied, err := s.authorizeReads(ctx, scope, mp.FunctionName, mp.EventSourceArn)
	switch {
	case err == nil:
		return nil
	case denied.Service == serviceSQS:
		return invalid("The provided execution role does not have permissions to call %s on SQS", denied.Operation)
	}

	return invalid("Cannot access stream %s. Please ensure the role can perform the %s Actions on your stream in IAM.",
		mp.EventSourceArn, actionList(eventsource.Reads(mp.EventSourceArn)))
}

// actionList names the operations of calls, as AWS lists the actions a role needs.
func actionList(calls []catalog.DeliveryRole) string {
	names := make([]string, len(calls))
	for i := range calls {
		names[i] = calls[i].Operation
	}

	return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
}
