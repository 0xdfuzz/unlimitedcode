package rotate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeGate struct {
	mu     sync.Mutex
	events *[]string
}

func (g *fakeGate) record(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	*g.events = append(*g.events, event)
}

func (g *fakeGate) Pause()        { g.record("pause") }
func (g *fakeGate) Resume()       { g.record("resume") }
func (g *fakeGate) CloseTunnels() { g.record("close") }

type fakeReconnector struct {
	mu     sync.Mutex
	events *[]string
	calls  int
	delay  time.Duration
	block  chan struct{}
	err    error
}

func (r *fakeReconnector) Name() string { return "fake" }

func (r *fakeReconnector) Reconnect(ctx context.Context) error {
	r.mu.Lock()
	r.calls++
	block := r.block
	delay := r.delay
	err := r.err
	if r.events != nil {
		*r.events = append(*r.events, "reconnect")
	}
	r.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (r *fakeReconnector) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func newManager(t *testing.T, gate Gate, rec Reconnector) *Manager {
	t.Helper()
	m, err := New(Options{
		Gate:        gate,
		Reconnector: rec,
		Timeout:     2 * time.Second,
		Cooldown:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHopOrderAndStats(t *testing.T) {
	var events []string
	gate := &fakeGate{events: &events}
	rec := &fakeReconnector{events: &events}
	m := newManager(t, gate, rec)

	result := m.Hop(context.Background(), Request{Reason: "test", Model: "m"})
	if result.Status != "ok" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"pause", "close", "reconnect", "resume"}
	if len(events) != len(want) {
		t.Fatalf("events = %v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
	stats := m.Stats()
	if stats.Count != 1 || stats.Failures != 0 || stats.LastStatus != "ok" || stats.Busy {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestHopCoalesces(t *testing.T) {
	gate := &fakeGate{events: &[]string{}}
	block := make(chan struct{})
	rec := &fakeReconnector{block: block}
	m := newManager(t, gate, rec)

	const waiters = 4
	results := make(chan Result, waiters)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		go func() {
			<-start
			results <- m.Hop(context.Background(), Request{Reason: "test"})
		}()
	}
	close(start)

	deadline := time.Now().Add(time.Second)
	for !m.Stats().Busy && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(block)

	coalesced := 0
	for i := 0; i < waiters; i++ {
		result := <-results
		if result.Status != "ok" {
			t.Fatalf("result = %+v", result)
		}
		if result.Coalesced {
			coalesced++
		}
	}
	if got := rec.callCount(); got != 1 {
		t.Fatalf("reconnects = %d, want 1", got)
	}
	if coalesced == 0 {
		t.Fatal("expected at least one coalesced result")
	}
}

func TestHopFailureCooldown(t *testing.T) {
	gate := &fakeGate{events: &[]string{}}
	rec := &fakeReconnector{err: errors.New("boom")}
	m := newManager(t, gate, rec)

	first := m.Hop(context.Background(), Request{Reason: "test"})
	if first.Status != "failed" {
		t.Fatalf("first = %+v", first)
	}
	second := m.Hop(context.Background(), Request{Reason: "test"})
	if second.Status != "failed" {
		t.Fatalf("second = %+v", second)
	}
	if rec.callCount() != 1 {
		t.Fatalf("reconnects = %d, want 1", rec.callCount())
	}
	if stats := m.Stats(); stats.Failures != 1 || stats.LastError == "" {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestHopWithoutReconnector(t *testing.T) {
	gate := &fakeGate{events: &[]string{}}
	m := newManager(t, gate, nil)
	result := m.Hop(context.Background(), Request{Reason: "test"})
	if result.Status != "failed" {
		t.Fatalf("result = %+v", result)
	}
}

func TestHopRateLimit(t *testing.T) {
	gate := &fakeGate{events: &[]string{}}
	rec := &fakeReconnector{}
	now := time.Unix(1000, 0)
	m, err := New(Options{
		Gate:        gate,
		Reconnector: rec,
		Timeout:     time.Second,
		MaxPerHour:  2,
		MaxPerDay:   10,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if result := m.Hop(context.Background(), Request{Reason: "test"}); result.Status != "ok" {
			t.Fatalf("hop %d = %+v", i, result)
		}
	}
	limited := m.Hop(context.Background(), Request{Reason: "test"})
	if limited.Status != "failed" || limited.Error == "" {
		t.Fatalf("limited = %+v", limited)
	}
	if rec.callCount() != 2 {
		t.Fatalf("reconnects = %d, want 2", rec.callCount())
	}
	if stats := m.Stats(); stats.Limited != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	now = now.Add(time.Hour + time.Second)
	if result := m.Hop(context.Background(), Request{Reason: "test"}); result.Status != "ok" {
		t.Fatalf("after window = %+v", result)
	}
}
