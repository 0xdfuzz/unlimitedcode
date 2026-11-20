package resume

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type runCall struct {
	binary string
	args   []string
	env    []string
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []runCall
	start   chan struct{}
	release chan struct{}
	err     error
}

func (r *fakeRunner) Run(ctx context.Context, binary string, args []string, env []string) error {
	r.mu.Lock()
	r.calls = append(r.calls, runCall{
		binary: binary,
		args:   append([]string(nil), args...),
		env:    append([]string(nil), env...),
	})
	start, release, err := r.start, r.release, r.err
	r.mu.Unlock()

	if start != nil {
		select {
		case start <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeRunner) call(index int) runCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[index]
}

func TestResumeSendsContinue(t *testing.T) {
	runner := &fakeRunner{}
	m, err := New(Options{
		Binary: "opencode",
		Env:    []string{"X=1"},
		Runner: runner,
		Now:    func() time.Time { return time.Unix(1000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background(), "ses_abc123"); err != nil {
		t.Fatal(err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("calls = %d", runner.callCount())
	}
	call := runner.call(0)
	if call.binary != "opencode" {
		t.Fatalf("binary = %q", call.binary)
	}
	want := []string{"run", "--session", "ses_abc123", "continue"}
	if !reflect.DeepEqual(call.args, want) {
		t.Fatalf("args = %v, want %v", call.args, want)
	}
	for _, arg := range call.args {
		if arg == "--auto" || arg == "--standalone" {
			t.Fatalf("forbidden flag %q used", arg)
		}
	}
	if len(call.env) != 1 || call.env[0] != "X=1" {
		t.Fatalf("env = %v", call.env)
	}
	stats := m.Stats()
	if stats.Count != 1 || stats.Failures != 0 || stats.LastSession != "ses_abc123" || stats.LastAt == nil {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestResumeRejectsInvalidSession(t *testing.T) {
	runner := &fakeRunner{}
	m, err := New(Options{Binary: "opencode", Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	invalid := []string{"", "ses_", "ses_a b", "--auto", "abc123", "ses_abc;rm"}
	for _, session := range invalid {
		if err := m.Resume(context.Background(), session); err == nil {
			t.Errorf("Resume(%q) should fail", session)
		}
	}
	if runner.callCount() != 0 {
		t.Fatalf("calls = %d", runner.callCount())
	}
}

func TestResumeRejectsConcurrentSameSession(t *testing.T) {
	start := make(chan struct{}, 1)
	release := make(chan struct{})
	runner := &fakeRunner{start: start, release: release}
	m, err := New(Options{Binary: "opencode", Runner: runner, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() { first <- m.Resume(context.Background(), "ses_busy1") }()
	select {
	case <-start:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	if err := m.Resume(context.Background(), "ses_busy1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second resume = %v, want ErrBusy", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	stats := m.Stats()
	if stats.Count != 1 || stats.Busy != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestResumeFailureStats(t *testing.T) {
	runner := &fakeRunner{err: errors.New("boom")}
	m, err := New(Options{Binary: "opencode", Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background(), "ses_fail1"); err == nil {
		t.Fatal("expected failure")
	}
	stats := m.Stats()
	if stats.Failures != 1 || stats.LastError == "" || stats.Count != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestResumeRequiresBinary(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing binary must fail")
	}
}
