package pipes

import (
	"context"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/eventsource"
)

// readCheckInterval is how long a check of a pipe's source reads stands. The runner visits every pipe each second,
// and a check records itself in the request inspector, so an idle pipe would otherwise fill it.
var readCheckInterval = 30 * time.Second

// readCheck is the result of a check of a pipe's reads, for the role and source it was made for.
type readCheck struct {
	until        time.Time
	err          error
	role, source string
}

// checks keeps the latest check of each pipe, by its ARN.
type checks struct {
	pipes map[string]readCheck
	mu    sync.Mutex
}

// authorizeReads checks that the pipe's role may read its source, as the pipe's role does in AWS. A denied read
// leaves the records where they are, and the next visit after the check expires tries again.
func (s *Service) authorizeReads(ctx context.Context, scope request.Scope, p *pipe) error {
	arn := pipeARN(scope, p.Name)
	s.checked.mu.Lock()
	defer s.checked.mu.Unlock()
	if c, ok := s.checked.pipes[arn]; ok && c.role == p.RoleArn && c.source == p.Source && s.now().Before(c.until) {
		return c.err
	}

	var err error
	reads := eventsource.Reads(p.Source)
	for i := range reads {
		read := reads[i]
		read.ARN, read.Session = p.RoleArn, p.Name
		err = s.authorizer.Authorize(ctx, catalog.ServiceDelivery{
			Principal: "pipes.amazonaws.com", SourceARN: arn, SourceAccount: scope.AccountID, Role: &read,
		})
		if err != nil {
			break
		}
	}

	if s.checked.pipes == nil {
		s.checked.pipes = map[string]readCheck{}
	}

	s.checked.pipes[arn] = readCheck{
		until: s.now().Add(readCheckInterval), err: err, role: p.RoleArn, source: p.Source,
	}

	return err
}
