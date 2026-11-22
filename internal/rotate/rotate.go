// Package rotate changes the egress of the local proxy.
//
// A hop is a single-flight operation. Concurrent callers wait for the hop in
// progress instead of starting another reconnect, so a burst of quota errors
// produces exactly one network change.
package rotate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Gate is the proxy surface used during a hop.
type Gate interface {
	Pause()
	Resume()
	CloseTunnels()
}

// Reconnector changes the underlying egress and waits until it is usable.
type Reconnector interface {
	Name() string
	Reconnect(ctx context.Context) error
}

type Logger interface {
	Printf(format string, args ...any)
}

type Options struct {
	Gate        Gate
	Reconnector Reconnector
	Timeout     time.Duration
	Cooldown    time.Duration
	MaxPerHour  int
	MaxPerDay   int
	StatePath   string
	Logger      Logger
	Now         func() time.Time
}

// Request carries metadata from the client. Prompts and bodies never reach
// this type.
type Request struct {
	Session string
	Model   string
	Agent   string
	Reason  string
	Attempt int
}

type Result struct {
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Coalesced  bool   `json:"coalesced,omitempty"`
	Epoch      int64  `json:"epoch,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Stats struct {
	Enabled        bool       `json:"enabled"`
	Busy           bool       `json:"busy"`
	Epoch          int64      `json:"epoch"`
	Count          int64      `json:"count"`
	Failures       int64      `json:"failures"`
	Coalesced      int64      `json:"coalesced"`
	Limited        int64      `json:"limited"`
	LastAt         *time.Time `json:"last_at,omitempty"`
	LastDurationMS int64      `json:"last_duration_ms,omitempty"`
	LastStatus     string     `json:"last_status,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
}

type Manager struct {
	gate          Gate
	rec           Reconnector
	opts          Options
	log           Logger
	now           func() time.Time
	ioMu          sync.Mutex
	wg            sync.WaitGroup
	mu            sync.Mutex
	run           *call
	epoch         int64
	lastF         time.Time
	lastOK        time.Time
	lastOKSession string
	lastOKAttempt int
	times         []time.Time
	stats         Stats
}

// transportGrace suppresses duplicate proxy.transport-error hops triggered by
// sibling sub-agents whose sockets were intentionally reset during a rotation
// that just succeeded.
const transportGrace = 5 * time.Second

// siblingFreeLimitGrace suppresses stale provider.free-limit hops arriving from
// OTHER parallel sub-agents right after a fast-rotate (1-2ms) completed on an
// older IP, preventing them from burning the brand-new slot before using it.
// A retry from the same session (higher attempt) still rotates immediately.
const siblingFreeLimitGrace = 2 * time.Second

type call struct {
	done   chan struct{}
	result Result
}

func New(opts Options) (*Manager, error) {
	if opts.Gate == nil {
		return nil, fmt.Errorf("hop: gate is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	if opts.Cooldown < 0 {
		opts.Cooldown = 0
	}
	if opts.MaxPerHour < 0 {
		opts.MaxPerHour = 0
	}
	if opts.MaxPerDay < 0 {
		opts.MaxPerDay = 0
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	m := &Manager{
		gate: opts.Gate,
		rec:  opts.Reconnector,
		opts: opts,
		log:  logger,
		now:  now,
	}
	m.loadState()
	return m, nil
}

type persistedState struct {
	Count int64 `json:"count"`
}

func (m *Manager) loadState() {
	if m.opts.StatePath == "" {
		return
	}
	raw, err := os.ReadFile(m.opts.StatePath)
	if err != nil {
		return
	}
	var st persistedState
	if json.Unmarshal(raw, &st) == nil && st.Count > 0 {
		m.stats.Count = st.Count
	}
}

func (m *Manager) saveState(count int64) {
	if m.opts.StatePath == "" {
		return
	}
	raw, err := json.Marshal(persistedState{Count: count})
	if err != nil {
		return
	}
	m.ioMu.Lock()
	defer m.ioMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(m.opts.StatePath), 0o700)
	_ = os.WriteFile(m.opts.StatePath, append(raw, '\n'), 0o600)
}

func (m *Manager) saveStateAsync(count int64) {
	if m.opts.StatePath == "" {
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.saveState(count)
	}()
}

// Flush waits for any in-flight background state writes and persists the final
// rotation counter to disk on daemon shutdown.
func (m *Manager) Flush() {
	m.wg.Wait()
	m.mu.Lock()
	count := m.stats.Count
	m.mu.Unlock()
	if count > 0 {
		m.saveState(count)
	}
}

// Hop runs one egress change or waits for the one already running. The
// reconnect uses its own timeout, so a caller that disconnects does not cancel
// a hop other sessions are waiting for.
func (m *Manager) Hop(ctx context.Context, req Request) Result {
	m.mu.Lock()
	if running := m.run; running != nil {
		m.stats.Coalesced++
		m.mu.Unlock()
		select {
		case <-running.done:
			result := running.result
			result.Coalesced = true
			return result
		case <-ctx.Done():
			return Result{Status: "failed", Error: "waiting for hop: " + ctx.Err().Error()}
		}
	}

	now := m.now()
	if !m.lastOK.IsZero() {
		sinceOK := now.Sub(m.lastOK)
		if req.Reason == "proxy.transport-error" && sinceOK < transportGrace {
			m.stats.Coalesced++
			ep := m.epoch
			m.mu.Unlock()
			return Result{Status: "ok", Coalesced: true, Epoch: ep}
		}
		if req.Reason == "provider.free-limit" && sinceOK < siblingFreeLimitGrace &&
			req.Session != "" && m.lastOKSession != "" &&
			req.Session != m.lastOKSession && req.Attempt <= m.lastOKAttempt {
			m.stats.Coalesced++
			ep := m.epoch
			m.mu.Unlock()
			return Result{Status: "ok", Coalesced: true, Epoch: ep}
		}
	}

	if !m.lastF.IsZero() && m.opts.Cooldown > 0 {
		if waited := now.Sub(m.lastF); waited < m.opts.Cooldown {
			remaining := m.opts.Cooldown - waited
			m.mu.Unlock()
			return Result{
				Status: "failed",
				Error:  fmt.Sprintf("hop cooldown %s", remaining.Round(time.Millisecond)),
			}
		}
	}

	if m.limitedLocked(now) {
		m.stats.Limited++
		m.mu.Unlock()
		return Result{Status: "failed", Error: "hop rate limit reached"}
	}

	running := &call{done: make(chan struct{})}
	m.run = running
	m.times = append(m.times, now)
	m.mu.Unlock()

	result := m.execute(req)

	m.mu.Lock()
	m.stats.Count++
	m.stats.LastStatus = result.Status
	m.stats.LastDurationMS = result.DurationMS
	m.stats.LastError = result.Error
	finished := m.now()
	m.stats.LastAt = &finished
	if result.Status == "ok" {
		m.epoch++
		m.stats.Epoch = m.epoch
		result.Epoch = m.epoch
		m.lastF = time.Time{}
		m.lastOK = finished
		m.lastOKSession = req.Session
		m.lastOKAttempt = req.Attempt
	} else {
		m.stats.Failures++
		m.lastF = finished
	}
	m.run = nil
	running.result = result
	close(running.done)
	savedCount := m.stats.Count
	m.mu.Unlock()
	m.saveStateAsync(savedCount)

	return result
}

func (m *Manager) execute(req Request) Result {
	start := m.now()
	if m.rec == nil {
		return Result{Status: "failed", Error: "hop is not configured"}
	}
	m.log.Printf("hop start reason=%s model=%s attempt=%d", req.Reason, req.Model, req.Attempt)

	m.gate.Pause()
	defer m.gate.Resume()
	// Automatic plugin rotations switch Bun's proxy URL epoch (http://r<epoch>@...)
	// so new requests immediately open fresh TCP tunnels on the new slot without
	// murdering active streaming sub-agents on the old slot (which drains cleanly).
	// Manual/CLI rotations still close tunnels for non-epoch callers.
	if req.Reason != "provider.free-limit" && req.Reason != "proxy.transport-error" {
		m.gate.CloseTunnels()
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.opts.Timeout)
	defer cancel()
	err := m.rec.Reconnect(ctx)
	duration := m.now().Sub(start)
	if err != nil {
		m.log.Printf("hop failed dur=%s: %v", duration.Round(time.Millisecond), err)
		return Result{Status: "failed", DurationMS: duration.Milliseconds(), Error: err.Error()}
	}
	m.log.Printf("hop ok dur=%s via=%s", duration.Round(time.Millisecond), m.rec.Name())
	return Result{Status: "ok", DurationMS: duration.Milliseconds()}
}

func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := m.stats
	stats.Enabled = m.rec != nil
	stats.Busy = m.run != nil
	return stats
}

// limitedLocked enforces the rolling hop budget. Zero disables a limit.
func (m *Manager) limitedLocked(now time.Time) bool {
	hourStart := now.Add(-time.Hour)
	dayStart := now.Add(-24 * time.Hour)
	kept := m.times[:0]
	lastHour := 0
	for _, at := range m.times {
		if at.After(dayStart) {
			kept = append(kept, at)
			if at.After(hourStart) {
				lastHour++
			}
		}
	}
	m.times = kept
	if m.opts.MaxPerHour > 0 && lastHour >= m.opts.MaxPerHour {
		return true
	}
	if m.opts.MaxPerDay > 0 && len(m.times) >= m.opts.MaxPerDay {
		return true
	}
	return false
}
