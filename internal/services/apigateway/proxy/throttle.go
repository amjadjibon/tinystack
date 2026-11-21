package proxy

import "time"

const (
	DefaultThrottleBurst = 5000
	DefaultThrottleRate  = 10000.0
)

// Bucket implements API Gateway's token bucket. The caller serializes access
// and stores the returned bucket when admitting a request.
type Bucket struct {
	at                  time.Time
	rate, burst, tokens float64
}

func (b Bucket) Take(now time.Time, rate, burst float64) (Bucket, bool) {
	if b.at.IsZero() || b.burst != burst || b.rate != rate {
		b = Bucket{at: now, rate: rate, burst: burst, tokens: burst}
	}

	if now.After(b.at) {
		b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*rate)
		b.at = now
	}

	if b.tokens < 1 {
		return b, false
	}

	b.tokens--
	return b, true
}
