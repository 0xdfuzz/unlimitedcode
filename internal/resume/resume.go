// Package resume runs the official OpenCode binary to continue a session.
//
// It is the fallback path: the retry plugin keeps the current turn alive, and
// resume only exists for setups without the plugin. It never uses --standalone
// or --auto and never approves permissions on the user's behalf.
package resume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"
)

// ErrBusy means another resume for the same session is still running.
var ErrBusy = errors.New("resume already running for session")

var sessionPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

type Logger interface {
	Printf(format string, args ...any)
}

type Runner interface {
	Run(ctx context.Context, binary string, args []string, env []string) error
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, binary string, args []string, env []string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	hideWindow(cmd)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("opencode run exited %d", exitErr.ExitCode())
	}
	return err
}

type Options struct {
	Binary  string
	Env     []string
	Timeout time.Duration
	Logger  Logger
	Runner  Runner
	Now     func() time.Time
}

type Stats struct {
	Enabled     bool       `json:"enabled"`
	Binary      string     `json:"binary,omitempty"`
	Count       int64      `json:"count"`
	Failures    int64      `json:"failures"`
	Busy        int64      `json:"busy"`
	LastAt      *time.Time `json:"last_at,omitempty"`
	LastSession string     `json:"last_session,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type Manager struct {
	opts   Options
	logger Logger
	now    func() time.Time
	runner Runner

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	stats Stats
}

func New(opts Options) (*Manager, error) {
	if opts.Binary == "" {
		return nil, errors.New("resume: opencode binary is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	runner := opts.Runner
	if runner == nil {
		runner = execRunner{}
	}
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	return &Manager{
		opts:   opts,
		logger: logger,
		now:    now,
		runner: runner,
		locks:  make(map[string]*sync.Mutex),
		stats:  Stats{Enabled: true, Binary: opts.Binary},
	}, nil
}

// Resume continues session with the word "continue". Concurrent calls for the
// same session are rejected; other sessions may resume in parallel.
func (m *Manager) Resume(ctx context.Context, session string) error {
	if !sessionPattern.MatchString(session) {
		return fmt.Errorf("resume: invalid session id %q", session)
	}

	m.mu.Lock()
	lock := m.locks[session]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[session] = lock
	}
	m.mu.Unlock()
	if !lock.TryLock() {
		m.mu.Lock()
		m.stats.Busy++
		m.mu.Unlock()
		return ErrBusy
	}
	defer lock.Unlock()

	start := m.now()
	m.logger.Printf("resume start session=%s binary=%s", session, m.opts.Binary)
	runCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()

	err := m.runner.Run(runCtx, m.opts.Binary, []string{"run", "--session", session, "continue"}, m.opts.Env)

	duration := m.now().Sub(start)
	m.mu.Lock()
	m.stats.Count++
	now := m.now()
	m.stats.LastAt = &now
	m.stats.LastSession = session
	if err != nil {
		m.stats.Failures++
		m.stats.LastError = err.Error()
	} else {
		m.stats.LastError = ""
	}
	m.mu.Unlock()

	if err != nil {
		m.logger.Printf("resume failed session=%s dur=%s: %v", session, duration.Round(time.Millisecond), err)
		return err
	}
	m.logger.Printf("resume ok session=%s dur=%s", session, duration.Round(time.Millisecond))
	return nil
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}
