package warp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type runnerFunc func(ctx context.Context, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}

func TestReconnectSequence(t *testing.T) {
	var mu sync.Mutex
	state := "Connected"
	var calls []string
	runner := runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.Join(args, " ")
		calls = append(calls, key)
		switch key {
		case "--no-ansi -j status":
			return []byte(`{"status":"` + state + `","reason":"test"}`), nil
		case "--no-ansi disconnect":
			state = "Disconnected"
			return nil, nil
		case "--no-ansi tunnel rotate-keys", "--no-ansi tunnel endpoint reset":
			return nil, nil
		case "--no-ansi connect":
			state = "Connected"
			return nil, nil
		default:
			return nil, errors.New("unexpected command " + key)
		}
	})

	cli := &CLI{Run: runner, Poll: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"--no-ansi -j status",
		"--no-ansi disconnect",
		"--no-ansi -j status",
		"--no-ansi tunnel rotate-keys",
		"--no-ansi tunnel endpoint reset",
		"--no-ansi connect",
		"--no-ansi -j status",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, calls[i], want[i])
		}
	}
}

func TestReconnectAlreadyDisconnected(t *testing.T) {
	var mu sync.Mutex
	state := "Disconnected"
	var calls []string
	runner := runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.Join(args, " ")
		calls = append(calls, key)
		switch key {
		case "--no-ansi -j status":
			return []byte(`{"status":"` + state + `"}`), nil
		case "--no-ansi tunnel rotate-keys", "--no-ansi tunnel endpoint reset":
			return nil, nil
		case "--no-ansi connect":
			state = "Connected"
			return nil, nil
		default:
			return nil, errors.New("unexpected command " + key)
		}
	})

	cli := &CLI{Run: runner, Poll: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cli.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 || calls[3] != "--no-ansi connect" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestReconnectConnectFailure(t *testing.T) {
	var mu sync.Mutex
	state := "Connected"
	runner := runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch strings.Join(args, " ") {
		case "--no-ansi -j status":
			return []byte(`{"status":"` + state + `"}`), nil
		case "--no-ansi disconnect":
			state = "Disconnected"
			return nil, nil
		case "--no-ansi tunnel rotate-keys", "--no-ansi tunnel endpoint reset":
			return nil, nil
		case "--no-ansi connect":
			return nil, errors.New("service unavailable")
		}
		return nil, errors.New("unexpected")
	})

	cli := &CLI{Run: runner, Poll: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := cli.Reconnect(ctx)
	if err == nil || !strings.Contains(err.Error(), "service unavailable") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusJSONAndText(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("{\"status\":\"Connected\",\"reason\":\"NetworkHealthy\"}"), nil
	})
	cli := &CLI{Run: runner}
	status, err := cli.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status != "Connected" {
		t.Fatalf("status = %q", status)
	}
}

func TestFindExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-cli")
	if err := os.WriteFile(path, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Find(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
}
