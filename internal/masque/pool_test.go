package masque

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2a09:bac5:b93:274b::3ea:4b", "2a09:bac5:b93:274b::/64"},
		{"2a09:bac5:b93:274b:1:2:3:4", "2a09:bac5:b93:274b::/64"},
		{"104.28.239.134", "104.28.239.134"},
	}
	for _, tc := range cases {
		if got := NormalizePrefix(tc.in); got != tc.want {
			t.Fatalf("NormalizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNextResetUTC(t *testing.T) {
	before := time.Date(2026, 11, 20, 0, 0, 30, 0, time.UTC)
	got := nextResetUTC(before)
	want := time.Date(2026, 11, 20, 0, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("before reset: got %s, want %s", got, want)
	}

	after := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	got = nextResetUTC(after)
	want = time.Date(2026, 11, 21, 0, 1, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("after reset: got %s, want %s", got, want)
	}
}

func TestFailedSlotAutoRecovery(t *testing.T) {
	origBackoffs := recoverBackoffs
	recoverBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	defer func() { recoverBackoffs = origBackoffs }()

	pool, err := NewPool(Options{StateDir: t.TempDir(), Size: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	pool.slots[0].mu.Lock()
	pool.slots[0].state = "failed"
	pool.slots[0].lastErr = "context deadline exceeded"
	pool.slots[0].mu.Unlock()

	pool.slots[1].mu.Lock()
	pool.slots[1].state = "ready"
	pool.slots[1].ip = "2a09:bac5:b93:1c8c::2d8:cd"
	pool.slots[1].prefix = "2a09:bac5:b93:1c8c::/64"
	pool.slots[1].mu.Unlock()
	pool.active = 1

	var tries atomic.Int32
	pool.recycleFn = func(ctx context.Context, s *slot, avoidPrefix string) error {
		n := tries.Add(1)
		if n == 1 {
			s.setFailed(errors.New("transient dns blip"))
			return errors.New("transient dns blip")
		}
		s.mu.Lock()
		s.state = "ready"
		s.ip = "2a09:bac5:b94:2222::1"
		s.prefix = "2a09:bac5:b94:2222::/64"
		s.lastErr = ""
		s.recycles++
		s.mu.Unlock()
		return nil
	}

	// Scheduling twice must be single-flight (only one recovery goroutine).
	pool.scheduleRecover(pool.slots[0])
	pool.scheduleRecover(pool.slots[0])

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.slots[0].mu.Lock()
		st := pool.slots[0].state
		pool.slots[0].mu.Unlock()
		if st == "ready" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	pool.slots[0].mu.Lock()
	defer pool.slots[0].mu.Unlock()
	if pool.slots[0].state != "ready" {
		t.Fatalf("expected slot-0 to recover to ready, got %q (tries=%d)", pool.slots[0].state, tries.Load())
	}
	if tries.Load() != 2 {
		t.Fatalf("expected exactly 2 recovery tries (single-flight), got %d", tries.Load())
	}
}

func TestReconnectHealsFailedSpare(t *testing.T) {
	origBackoffs := recoverBackoffs
	recoverBackoffs = []time.Duration{5 * time.Millisecond}
	defer func() { recoverBackoffs = origBackoffs }()

	pool, err := NewPool(Options{StateDir: t.TempDir(), Size: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// slot-0 is failed (e.g. exhausted earlier backoffs during a long outage),
	// slot-1 is active.
	pool.slots[0].mu.Lock()
	pool.slots[0].state = "failed"
	pool.slots[0].mu.Unlock()

	pool.slots[1].mu.Lock()
	pool.slots[1].state = "ready"
	pool.slots[1].ip = "2a09:bac5:b93:1c8c::1"
	pool.slots[1].prefix = "2a09:bac5:b93:1c8c::/64"
	pool.slots[1].mu.Unlock()
	pool.active = 1

	pool.recycleFn = func(ctx context.Context, s *slot, avoidPrefix string) error {
		s.mu.Lock()
		s.state = "ready"
		s.ip = "2a09:bac5:b99:9999::1"
		s.prefix = "2a09:bac5:b99:9999::/64"
		s.lastErr = ""
		s.recycles++
		s.mu.Unlock()
		return nil
	}

	if err := pool.Reconnect(context.Background()); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.slots[0].mu.Lock()
		st := pool.slots[0].state
		pool.slots[0].mu.Unlock()
		if st == "ready" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	pool.slots[0].mu.Lock()
	defer pool.slots[0].mu.Unlock()
	if pool.slots[0].state != "ready" {
		t.Fatalf("expected Reconnect to trigger recovery of failed spare slot-0, got %q", pool.slots[0].state)
	}
}

func TestBurntPersistence(t *testing.T) {
	dir := t.TempDir()
	p1, err := NewPool(Options{StateDir: dir, Size: 2})
	if err != nil {
		t.Fatalf("NewPool p1: %v", err)
	}
	p1.markBurnt("2a09:bac5:b91:305a::/64")
	p1.Close()

	p2, err := NewPool(Options{StateDir: dir, Size: 2})
	if err != nil {
		t.Fatalf("NewPool p2: %v", err)
	}
	defer p2.Close()

	if !p2.isBurnt("2a09:bac5:b91:305a::/64") {
		t.Fatal("expected burnt prefix to persist across Pool restart")
	}
}
