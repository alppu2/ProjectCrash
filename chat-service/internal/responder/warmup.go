package responder

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Warmer readies a Responder's provider before the first turn. A Responder
// that does not implement it has nothing to warm.
type Warmer interface {
	Warmup(ctx context.Context) error
}

// Below compose's OLLAMA_KEEP_ALIVE (30m), so a fresh entry never vouches for
// a model Ollama has already unloaded.
const warmFreshness = 20 * time.Minute

// Covers a cold VRAM load (~33s) with headroom, like newLLMClient's header wait.
const warmTimeout = 120 * time.Second

type warmFlight struct {
	done chan struct{}
	err  error
}

// warmCache shares one in-flight warmup among concurrent callers and reuses a
// success for warmFreshness. Failures are not cached. The zero value is ready.
type warmCache struct {
	now     func() time.Time // nil means time.Now
	timeout time.Duration    // zero means warmTimeout

	mu       sync.Mutex
	warmedAt time.Time
	flight   *warmFlight
}

func (c *warmCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// do runs fn unless a fresh success or an in-flight call already covers it.
// fn is detached from ctx: one caller leaving must not abort the others' wait.
func (c *warmCache) do(ctx context.Context, fn func(context.Context) error) error {
	c.mu.Lock()
	if !c.warmedAt.IsZero() && c.clock().Sub(c.warmedAt) < warmFreshness {
		c.mu.Unlock()
		return nil
	}
	f := c.flight
	if f == nil {
		f = &warmFlight{done: make(chan struct{})}
		c.flight = f
		go c.run(context.WithoutCancel(ctx), f, fn)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *warmCache) run(ctx context.Context, f *warmFlight, fn func(context.Context) error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = warmTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := fn(ctx)
	if err != nil && ctx.Err() != nil {
		// %v, not %w: ClassifyOutcome would read DeadlineExceeded as a hangup.
		err = fmt.Errorf("warmup exceeded %s: %v", timeout, err)
	}

	c.mu.Lock()
	if err == nil {
		c.warmedAt = c.clock()
	}
	c.flight = nil
	c.mu.Unlock()

	f.err = err
	close(f.done)
}
