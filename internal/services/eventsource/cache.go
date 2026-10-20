package eventsource

import (
	"context"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// ReadCheckInterval bounds how often pollers record source permission checks in the request log.
const ReadCheckInterval = 30 * time.Second

type readCheck struct {
	until        time.Time
	denied       catalog.DeliveryRole
	err          error
	role, source string
}

// ReadCache remembers successful and denied read checks by reader (mapping UUID or pipe ARN).
// The zero value is ready to use. Role or source changes force an immediate check.
type ReadCache struct {
	entries map[string]readCheck
	mu      sync.Mutex
}

// Authorize checks all source reads as d's principal and role, returning the first denied call.
// Concurrent checks are coalesced; denials are retried after ReadCheckInterval.
func (c *ReadCache) Authorize(ctx context.Context, authorizer catalog.ServiceAuthorizer,
	reader, source string, d catalog.ServiceDelivery, now time.Time,
) (catalog.DeliveryRole, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if check, ok := c.entries[reader]; ok && check.role == d.Role.ARN && check.source == source &&
		now.Before(check.until) {
		return check.denied, check.err
	}

	check := readCheck{role: d.Role.ARN, source: source, until: now.Add(ReadCheckInterval)}
	for _, read := range Reads(source) {
		read.ARN, read.Session = d.Role.ARN, d.Role.Session
		d.Role = &read
		if err := authorizer.Authorize(ctx, d); err != nil {
			if ctx.Err() != nil {
				return read, err
			}

			check.denied, check.err = read, err
			break
		}
	}

	if c.entries == nil {
		c.entries = map[string]readCheck{}
	}

	c.entries[reader] = check
	return check.denied, check.err
}

// Delete forgets the check of a deleted reader.
func (c *ReadCache) Delete(reader string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, reader)
}

// Reset clears every check before application state is reset.
func (c *ReadCache) Reset(context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}
