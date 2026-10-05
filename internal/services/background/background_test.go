package background

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroup(t *testing.T) {
	g := New()
	var ticks atomic.Int64
	g.Every(time.Millisecond, func(context.Context) { ticks.Add(1) })

	started := make(chan struct{})
	if !g.Go(func(ctx context.Context) { close(started); <-ctx.Done() }) {
		t.Fatal("Go refused before Stop")
	}

	<-started
	for ticks.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	if !g.Stop() || g.Stop() {
		t.Fatal("only the first Stop should report true")
	}

	if g.Context().Err() == nil {
		t.Fatal("Stop left the context running")
	}

	if g.Go(func(context.Context) { t.Error("ran after Stop") }) {
		t.Fatal("Go accepted work after Stop")
	}
}
