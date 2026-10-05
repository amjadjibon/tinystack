// Package background runs a service's goroutines until the service stops.
package background

import (
	"context"
	"sync"
	"time"
)

// Group runs goroutines on a context that Stop cancels, and Stop waits for them.
type Group struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	wg     sync.WaitGroup
	once   sync.Once
}

func New() *Group {
	ctx, cancel := context.WithCancel(context.Background())
	return &Group{ctx: ctx, cancel: cancel}
}

// Context is the context Stop cancels.
func (g *Group) Context() context.Context { return g.ctx }

// Go runs fn on the group's context and reports true, or, once Stop has begun, reports false without running it.
func (g *Group) Go(fn func(ctx context.Context)) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return false
	}

	g.wg.Go(func() { fn(g.ctx) })
	return true
}

// Every calls fn every interval until Stop.
func (g *Group) Every(interval time.Duration, fn func(ctx context.Context)) {
	g.Go(func(ctx context.Context) {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fn(ctx)
			}
		}
	})
}

// Stop cancels the group's context and waits for its goroutines. It reports true to the call that stopped the
// group; later calls wait for that one and report false.
func (g *Group) Stop() bool {
	first := false
	g.once.Do(func() {
		first = true
		g.mu.Lock()
		g.cancel()
		g.mu.Unlock()
		g.wg.Wait()
	})

	return first
}
