package budget

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

var noon = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		TokensPerVisitor:     1000,
		TokensGlobal:         5000,
		MaxVisitors:          10,
		MaxStreamsGlobal:     4,
		MaxStreamsPerVisitor: 2,
	}
}

func newTestLedger(cfg Config) (*Ledger, *fakeClock) {
	c := &fakeClock{t: noon}
	return New(cfg, c.Now), c
}

func refusal(t *testing.T, err error) *RefusedError {
	t.Helper()
	var r *RefusedError
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want *RefusedError", err)
	}
	return r
}

func TestDebitReportsTheBalance(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	release()

	b := l.Debit("a", 300)
	want := Balance{Used: 300, Remaining: 700, ResetsAt: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	if b != want {
		t.Errorf("Debit() = %+v, want %+v", b, want)
	}
}

// A visitor at the cap must be refused before the model runs, with the reset
// time the frontend shows.
func TestSpentVisitorIsRefused(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	l.Debit("a", 1000)

	_, err := l.Acquire("a")
	r := refusal(t, err)
	if r.Reason != Exhausted || !r.ResetsAt.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("refusal = %+v, want BUDGET_EXHAUSTED resetting at next UTC midnight", r)
	}
	if _, err := l.Acquire("b"); err != nil {
		t.Errorf("another visitor was refused: %v", err)
	}
}

// Charged after the turn, so one turn may overshoot; the next one must not start.
func TestOvershootIsBoundedToOneTurn(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	l.Debit("a", 999)
	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("a visitor with 1 token left was refused: %v", err)
	}
	release()
	if b := l.Debit("a", 800); b.Remaining != 0 || b.Used != 1799 {
		t.Errorf("balance = %+v, want used 1799, remaining clamped to 0", b)
	}
	if _, err := l.Acquire("a"); refusal(t, err).Reason != Exhausted {
		t.Error("the turn after an overshoot was not refused")
	}
}

// The provider spend cap is global; this is the layer that refuses before it.
func TestSpentSiteRefusesEveryone(t *testing.T) {
	cfg := testConfig()
	cfg.TokensGlobal = 1500
	l, _ := newTestLedger(cfg)
	l.Debit("a", 900)
	l.Debit("b", 600)

	_, err := l.Acquire("c")
	if r := refusal(t, err); r.Reason != GlobalExhausted || r.ResetsAt.IsZero() {
		t.Errorf("refusal = %+v, want GLOBAL_BUDGET_EXHAUSTED with a reset time", r)
	}
}

func TestDailyResetRestoresBudgets(t *testing.T) {
	l, clock := newTestLedger(testConfig())
	l.Debit("a", 1000)
	clock.t = time.Date(2026, 10, 12, 0, 0, 1, 0, time.UTC)

	release, err := l.Acquire("a")
	if err != nil {
		t.Fatalf("refused after midnight UTC: %v", err)
	}
	release()
	if b := l.Balance("a"); b.Used != 0 || b.Remaining != 1000 {
		t.Errorf("balance after reset = %+v, want a full budget", b)
	}
}

// If midnight reset open-stream counts, every visitor could open a fresh pair
// of streams at 00:00 on top of the ones still running.
func TestMidnightResetKeepsOpenStreams(t *testing.T) {
	l, clock := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")
	clock.t = time.Date(2026, 10, 12, 0, 0, 1, 0, time.UTC)

	if _, err := l.Acquire("a"); refusal(t, err).Reason != TooManyStreams {
		t.Error("midnight reset freed stream slots that are still open")
	}
	if b := l.Debit("a", 50); b.Used != 50 {
		t.Errorf("a debit straddling midnight = %+v, want it charged to the new day", b)
	}
	r1()
	r2()
}

func TestStreamCaps(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")

	_, err := l.Acquire("a")
	if r := refusal(t, err); r.Reason != TooManyStreams || !r.ResetsAt.IsZero() {
		t.Errorf("refusal = %+v, want TOO_MANY_STREAMS without a reset time", r)
	}

	r3, _ := l.Acquire("b")
	r4, _ := l.Acquire("b")
	if _, err := l.Acquire("c"); refusal(t, err).Reason != TooManyStreams {
		t.Error("the site-wide stream cap did not hold")
	}

	r1()
	if _, err := l.Acquire("a"); err != nil {
		t.Errorf("a released slot was not reusable: %v", err)
	}
	r2()
	r3()
	r4()
}

// release is deferred and may also run on an error path; a second call must
// not free someone else's slot.
func TestReleaseIsIdempotent(t *testing.T) {
	l, _ := newTestLedger(testConfig())
	r1, _ := l.Acquire("a")
	r2, _ := l.Acquire("a")
	r1()
	r1()

	if _, err := l.Acquire("a"); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	if _, err := l.Acquire("a"); refusal(t, err).Reason != TooManyStreams {
		t.Error("a double release freed two slots")
	}
	r2()
}

// A spray of fresh addresses must not grow memory without bound: past the
// cap, new visitors are refused (fail closed) and known ones still served.
func TestVisitorCapFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.MaxVisitors = 2
	l, _ := newTestLedger(cfg)
	for _, k := range []string{"a", "b"} {
		r, err := l.Acquire(k)
		if err != nil {
			t.Fatal(err)
		}
		r()
	}

	if _, err := l.Acquire("c"); refusal(t, err).Reason != GlobalExhausted {
		t.Error("a visitor past the cap was admitted")
	}
	r, err := l.Acquire("a")
	if err != nil {
		t.Errorf("a known visitor was refused at the cap: %v", err)
	}
	r()
}

func TestBalanceDoesNotRegisterAVisitor(t *testing.T) {
	cfg := testConfig()
	cfg.MaxVisitors = 1
	l, _ := newTestLedger(cfg)
	l.Balance("crawler")

	if _, err := l.Acquire("a"); err != nil {
		t.Errorf("a Balance lookup took a visitor slot: %v", err)
	}
}

func TestVisitorKey(t *testing.T) {
	tests := []struct {
		addr, want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		// Dual-stack listeners report IPv4 clients this way.
		{"::ffff:203.0.113.7", "203.0.113.7"},
		// One host owns a whole /64 and could rotate through it.
		{"2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:bbbb::9", "2001:db8:1:2::/64"},
	}
	for _, tt := range tests {
		got, err := VisitorKey(tt.addr)
		if err != nil || got != tt.want {
			t.Errorf("VisitorKey(%q) = %q, %v; want %q", tt.addr, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "unknown", "203.0.113.7:443"} {
		if _, err := VisitorKey(bad); err == nil {
			t.Errorf("VisitorKey(%q) succeeded, want an error", bad)
		}
	}
}
