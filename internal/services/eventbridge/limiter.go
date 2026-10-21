package eventbridge

import (
	"context"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
)

// rateLimiters keeps per-destination token buckets keyed by destination ARN.
type rateLimiters struct {
	buckets map[string]*bucket
	mu      sync.Mutex
}

// bucket is an in-memory token bucket limiter for one destination.
type bucket struct {
	last   time.Time
	rate   float64
	burst  float64
	tokens float64
	mu     sync.Mutex
}

func (l *rateLimiters) get(arn string, rateLimit int64) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]*bucket)
	}

	b, ok := l.buckets[arn]
	if !ok {
		limit := float64(rateLimit)
		b = &bucket{
			rate:   limit,
			burst:  limit,
			tokens: limit,
			last:   time.Now(),
		}
		l.buckets[arn] = b
	}

	return b
}

func (l *rateLimiters) wait(ctx context.Context, arn string, rateLimit int64, timeout time.Duration) error {
	b := l.get(arn, rateLimit)

	b.mu.Lock()
	now := time.Now()
	if b.last.IsZero() {
		b.last = now
		b.tokens = b.burst
	}

	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}

	b.last = now

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		b.mu.Unlock()
		return nil
	}

	needed := 1.0 - b.tokens
	delay := time.Duration((needed / b.rate) * float64(time.Second))
	if delay > timeout {
		b.mu.Unlock()
		return awserror.New(codeRetryable, "API destination rate limit exceeded.")
	}

	b.tokens -= 1.0
	b.mu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		b.mu.Lock()
		b.tokens += 1.0
		b.mu.Unlock()
		return ctx.Err()
	}
}

func (l *rateLimiters) update(arn string, rateLimit int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[arn]
	if !ok {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if !b.last.IsZero() {
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * b.rate
		b.last = now
	}

	limit := float64(rateLimit)
	b.rate = limit
	b.burst = limit
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

func (l *rateLimiters) drop(arn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, arn)
}
