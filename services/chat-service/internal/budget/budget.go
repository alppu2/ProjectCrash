// Package budget caps the tokens each visitor and the whole site may spend per
// UTC day, and the streams they may hold open. State is in memory: a restart
// resets it, and the provider's own spend cap is the backstop.
package budget

import (
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// Reason is wire contract: the frontend picks its copy by it.
type Reason string

const (
	TooManyStreams  Reason = "TOO_MANY_STREAMS"
	Exhausted       Reason = "BUDGET_EXHAUSTED"
	GlobalExhausted Reason = "GLOBAL_BUDGET_EXHAUSTED"
)

// RefusedError is returned by Acquire. ResetsAt is zero for TooManyStreams: a
// slot frees when a stream ends, not at midnight.
type RefusedError struct {
	Reason   Reason
	ResetsAt time.Time
}

func (e *RefusedError) Error() string { return "budget: refused, " + string(e.Reason) }

type Config struct {
	TokensPerVisitor     int64
	TokensGlobal         int64
	MaxVisitors          int
	MaxStreamsGlobal     int
	MaxStreamsPerVisitor int
}

type Balance struct {
	Used, Remaining int64
	ResetsAt        time.Time
}

type Ledger struct {
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	day    time.Time // UTC midnight the token counters belong to
	used   map[string]int64
	global int64
	// Open streams outlive the day boundary, so rollover leaves them alone.
	open      map[string]int
	openTotal int
}

func New(cfg Config, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{cfg: cfg, now: now, used: map[string]int64{}, open: map[string]int{}}
}

// rollover starts a new day's counters when the UTC date has moved on, and
// returns the next reset. Callers hold mu.
func (l *Ledger) rollover() time.Time {
	t := l.now().UTC()
	today := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if !today.Equal(l.day) {
		l.day = today
		l.used = map[string]int64{}
		l.global = 0
		budgetGlobalUsed.Set(0)
	}
	return today.AddDate(0, 0, 1)
}

// Acquire takes a stream slot for key if the caps and today's budgets allow.
// The returned release is idempotent; defer it.
func (l *Ledger) Acquire(key string) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	resets := l.rollover()

	if l.openTotal >= l.cfg.MaxStreamsGlobal || l.open[key] >= l.cfg.MaxStreamsPerVisitor {
		return nil, refuse(TooManyStreams, time.Time{})
	}
	used, known := l.used[key]
	switch {
	case l.global >= l.cfg.TokensGlobal:
		return nil, refuse(GlobalExhausted, resets)
	case !known && len(l.used) >= l.cfg.MaxVisitors:
		// Fail closed, like the guard: a spray of fresh addresses must not
		// grow the map without bound.
		return nil, refuse(GlobalExhausted, resets)
	case used >= l.cfg.TokensPerVisitor:
		return nil, refuse(Exhausted, resets)
	}

	l.used[key] = used
	l.open[key]++
	l.openTotal++
	var once sync.Once
	return func() { once.Do(func() { l.release(key) }) }, nil
}

func (l *Ledger) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.openTotal--
	l.open[key]--
	if l.open[key] <= 0 {
		delete(l.open, key)
	}
}

func refuse(r Reason, resets time.Time) *RefusedError {
	budgetRefusalsTotal.WithLabelValues(string(r)).Inc()
	return &RefusedError{Reason: r, ResetsAt: resets}
}

// Debit charges tokens the provider reported for one turn and returns the
// visitor's balance after it.
func (l *Ledger) Debit(key string, tokens int64) Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	resets := l.rollover()
	if tokens > 0 {
		l.used[key] += tokens
		l.global += tokens
		budgetGlobalUsed.Set(float64(l.global))
		budgetVisitorTokens.Observe(float64(l.used[key]))
	}
	return l.balance(key, resets)
}

// Balance reads key's balance without registering it as a visitor.
func (l *Ledger) Balance(key string) Balance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.balance(key, l.rollover())
}

func (l *Ledger) balance(key string, resets time.Time) Balance {
	used := l.used[key]
	return Balance{Used: used, Remaining: max(0, l.cfg.TokensPerVisitor-used), ResetsAt: resets}
}

// VisitorKey maps a client address to the key budgets are kept under. IPv6 is
// truncated to its /64, which one host controls.
func VisitorKey(addr string) (string, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return "", fmt.Errorf("client address %q: %w", addr, err)
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String(), nil
	}
	p, err := a.Prefix(64)
	if err != nil {
		return "", fmt.Errorf("client address %q: %w", addr, err)
	}
	return p.String(), nil
}
