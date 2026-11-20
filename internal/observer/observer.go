// Package observer tails the OpenCode log and reports quota exhaustion.
//
// It is the fallback detector: the plugin removes the 429 from the retry path,
// while the observer notices the error after the fact. Only log lines are read;
// prompt text is never stored or logged. The first run starts at the current
// end of the file so old errors are not replayed.
package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion = 1
	maxLine      = 1 << 20
	maxRead      = 4 << 20
)

type Logger interface {
	Printf(format string, args ...any)
}

type Options struct {
	Path      string
	StatePath string
	Poll      time.Duration
	Dedup     time.Duration
	Logger    Logger
	OnQuota   func(session string)
	Suppress  func() bool
	Now       func() time.Time
}

type Stats struct {
	Enabled    bool       `json:"enabled"`
	Path       string     `json:"path,omitempty"`
	Offset     int64      `json:"offset"`
	Events     int64      `json:"events"`
	Duplicates int64      `json:"duplicates"`
	Suppressed int64      `json:"suppressed"`
	Errors     int64      `json:"errors"`
	LastAt     *time.Time `json:"last_at,omitempty"`
	LastTarget string     `json:"last_session,omitempty"`
}

type persisted struct {
	Version int    `json:"version"`
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
}

type Observer struct {
	opts   Options
	logger Logger
	now    func() time.Time

	scanMu  sync.Mutex
	mu      sync.Mutex
	offset  int64 // -1 until the first successful scan
	seen    map[string]time.Time
	stats   Stats
	lastErr string
}

func New(opts Options) (*Observer, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, errors.New("observer: log path is required")
	}
	if opts.Poll <= 0 {
		opts.Poll = 2 * time.Second
	}
	if opts.Dedup < 0 {
		opts.Dedup = 0
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	o := &Observer{
		opts:   opts,
		logger: logger,
		now:    now,
		offset: -1,
		seen:   make(map[string]time.Time),
		stats:  Stats{Enabled: true, Path: opts.Path},
	}
	o.loadState()
	if _, err := os.Stat(opts.Path); errors.Is(err, os.ErrNotExist) {
		// The log did not exist at startup. There is no history to skip, so
		// drop any stale offset and read from the beginning when it appears.
		o.setOffset(0)
		o.saveState()
	}
	return o, nil
}

func (o *Observer) loadState() {
	if o.opts.StatePath == "" {
		return
	}
	raw, err := os.ReadFile(o.opts.StatePath)
	if err != nil {
		return
	}
	var saved persisted
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Version != stateVersion {
		return
	}
	if saved.Path != o.opts.Path {
		return
	}
	o.offset = saved.Offset
	o.mu.Lock()
	o.stats.Offset = saved.Offset
	o.mu.Unlock()
}

// Run tails the log until ctx is cancelled.
func (o *Observer) Run(ctx context.Context) {
	o.poll()
	ticker := time.NewTicker(o.opts.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.poll()
		}
	}
}

// ScanOnce processes new lines once. It is used by tests.
func (o *Observer) ScanOnce() error {
	return o.scan()
}

func (o *Observer) poll() {
	if err := o.scan(); err != nil {
		o.mu.Lock()
		o.stats.Errors++
		changed := err.Error() != o.lastErr
		o.lastErr = err.Error()
		o.mu.Unlock()
		if changed {
			o.logger.Printf("observer: %v", err)
		}
	}
}

func (o *Observer) scan() error {
	o.scanMu.Lock()
	defer o.scanMu.Unlock()

	info, err := os.Stat(o.opts.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// OpenCode has not created the log yet, or rotated it away.
			// Nothing to read until it appears.
			return nil
		}
		return err
	}
	size := info.Size()

	o.mu.Lock()
	offset := o.offset
	o.mu.Unlock()

	if offset < 0 {
		// First run: skip history.
		offset = size
		o.setOffset(offset)
		o.saveState()
		return nil
	}
	if size < offset {
		o.logger.Printf("observer: log truncated, restarting from zero")
		o.mu.Lock()
		o.seen = make(map[string]time.Time)
		o.mu.Unlock()
		offset = 0
		o.setOffset(offset)
		o.saveState()
	}
	if size == offset {
		return nil
	}

	file, err := os.Open(o.opts.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	buf := make([]byte, maxRead)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	if n == 0 {
		return nil
	}
	data := buf[:n]

	lastNL := bytes.LastIndexByte(data, '\n')
	if lastNL < 0 {
		if n == len(buf) {
			// A single line larger than the read window; skip it rather than
			// stalling the tail forever.
			o.setOffset(offset + int64(n))
			o.saveState()
			o.mu.Lock()
			o.stats.Errors++
			o.mu.Unlock()
			return nil
		}
		// Wait for the line to complete.
		return nil
	}

	for _, line := range bytes.Split(data[:lastNL], []byte{'\n'}) {
		if len(line) == 0 || len(line) > maxLine {
			continue
		}
		if !bytes.Contains(line, []byte(`Failed to drain Session`)) {
			continue
		}
		if !bytes.Contains(line, []byte("Free usage limit reached")) {
			continue
		}
		o.handle(sessionOf(line))
	}

	o.setOffset(offset + int64(lastNL) + 1)
	o.saveState()
	return nil
}

func (o *Observer) handle(session string) {
	now := o.now()
	key := session
	if key == "" {
		key = "unknown"
	}

	if o.opts.Suppress != nil && o.opts.Suppress() {
		o.mu.Lock()
		o.stats.Suppressed++
		o.mu.Unlock()
		return
	}

	o.mu.Lock()
	if o.opts.Dedup > 0 {
		if last, ok := o.seen[key]; ok && now.Sub(last) < o.opts.Dedup {
			o.stats.Duplicates++
			o.mu.Unlock()
			return
		}
	}
	o.seen[key] = now
	o.stats.Events++
	o.stats.LastAt = &now
	o.stats.LastTarget = session
	handler := o.opts.OnQuota
	o.mu.Unlock()

	o.logger.Printf("observer quota event session=%s", session)
	if handler != nil {
		handler(session)
	}
}

func (o *Observer) setOffset(offset int64) {
	o.mu.Lock()
	o.offset = offset
	o.stats.Offset = offset
	o.mu.Unlock()
}

func (o *Observer) saveState() {
	if o.opts.StatePath == "" {
		return
	}
	o.mu.Lock()
	payload := persisted{Version: stateVersion, Path: o.opts.Path, Offset: o.offset}
	o.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(o.opts.StatePath), 0o700); err != nil {
		return
	}
	tmp := o.opts.StatePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, o.opts.StatePath); err != nil {
		os.Remove(tmp)
	}
}

func (o *Observer) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stats
}

func sessionOf(line []byte) string {
	const key = "sessionID="
	index := bytes.Index(line, []byte(key))
	if index < 0 {
		return ""
	}
	value := line[index+len(key):]
	value = bytes.TrimPrefix(value, []byte(`"`))
	end := bytes.IndexAny(value, " \t\r\n\"")
	if end >= 0 {
		value = value[:end]
	}
	if !bytes.HasPrefix(value, []byte("ses_")) {
		return ""
	}
	return string(value)
}

// Path is a small helper for diagnostics.
func (o *Observer) Path() string {
	return o.opts.Path
}
