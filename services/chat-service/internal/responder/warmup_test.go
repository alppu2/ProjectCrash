package responder

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every page load calls Warmup. Without sharing, N visitors during a cold
// load would queue N completions on one GPU.
func TestWarmCacheSharesOneFlight(t *testing.T) {
	var c warmCache
	var calls atomic.Int32
	release := make(chan struct{})
	fn := func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.do(context.Background(), fn)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("do() error = %v, want nil", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fn ran %d times, want 1", got)
	}
}

// Fresh successes are free; stale ones must warm again or the cache vouches
// for a model Ollama has unloaded.
func TestWarmCacheReusesFreshSuccessOnly(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	c := warmCache{now: func() time.Time { return now }}
	calls := 0
	fn := func(context.Context) error { calls++; return nil }

	_ = c.do(context.Background(), fn)
	now = now.Add(warmFreshness - time.Second)
	_ = c.do(context.Background(), fn)
	if calls != 1 {
		t.Fatalf("calls within the window = %d, want 1", calls)
	}

	now = now.Add(2 * time.Second)
	_ = c.do(context.Background(), fn)
	if calls != 2 {
		t.Errorf("calls after the window = %d, want 2", calls)
	}
}

// A provider that recovers must be picked up by the next page load, not
// after a restart.
func TestWarmCacheDoesNotCacheFailure(t *testing.T) {
	var c warmCache
	results := []error{errors.New("provider down"), nil}
	calls := 0
	fn := func(context.Context) error { err := results[calls]; calls++; return err }

	if err := c.do(context.Background(), fn); err == nil {
		t.Fatal("first do() error = nil, want the provider error")
	}
	if err := c.do(context.Background(), fn); err != nil {
		t.Errorf("second do() error = %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// A visitor closing the tab mid cold load must not abort the load every
// other visitor is waiting on.
func TestWarmCacheCallerCancelDoesNotCancelSharedWork(t *testing.T) {
	var c warmCache
	release := make(chan struct{})
	finished := make(chan error, 1)
	fn := func(ctx context.Context) error {
		<-release
		finished <- ctx.Err()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.do(ctx, fn) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}

	close(release)
	if err := <-finished; err != nil {
		t.Errorf("shared work saw ctx.Err() = %v, want nil", err)
	}
	// The flight finished successfully, so the next caller is served from cache.
	if err := c.do(context.Background(), func(context.Context) error {
		t.Error("fn ran again after a successful flight")
		return nil
	}); err != nil {
		t.Errorf("do() after flight error = %v, want nil", err)
	}
}

// A wedged provider must show up as an error, not as a client hangup:
// ClassifyOutcome maps DeadlineExceeded to "cancelled".
func TestWarmCacheTimeoutIsAnError(t *testing.T) {
	c := warmCache{timeout: 10 * time.Millisecond}
	err := c.do(context.Background(), func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("do() error = nil, want a timeout error")
	}
	if got := ClassifyOutcome(err); got != "error" {
		t.Errorf("ClassifyOutcome(%v) = %q, want %q", err, got, "error")
	}
}
