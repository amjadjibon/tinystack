package pipes

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// authorizeReads checks the pipe's source reads under its role, caching denials as well as successful checks.
func (s *Service) authorizeReads(ctx context.Context, scope request.Scope, p *pipe) error {
	arn := pipeARN(scope, p.Name)
	_, err := s.checked.Authorize(ctx, s.authorizer, arn, p.Source, catalog.ServiceDelivery{
		Principal: "pipes.amazonaws.com", SourceARN: arn, SourceAccount: scope.AccountID,
		Role: &catalog.DeliveryRole{ARN: p.RoleArn, Session: p.Name},
	}, s.now())
	return err
}

// Reset clears cached source permissions before application state is reset.
func (s *Service) Reset(ctx context.Context) { s.checked.Reset(ctx) }
