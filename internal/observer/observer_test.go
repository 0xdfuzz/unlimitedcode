package observer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	quotaLine = `timestamp=2026-11-19T22:47:28.764Z level=ERROR run=1 message="Failed to drain Session" cause="AI.Error: Free usage limit reached" role=server sessionID=ses_test123`
	plainLine = `timestamp=2026-11-19T22:47:28.764Z level=INFO message="something else"`
)

func newTestObserver(t *testing.T, dir string, dedup time.Duration, suppress func() bool) (*Observer, *[]string, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	events := &[]string{}
	obs, err := New(Options{
		Path:      filepath.Join(dir, "opencode.log"),
		StatePath: filepath.Join(dir, "observer.json"),
		Poll:      time.Millisecond,
		Dedup:     dedup,
		Suppress:  suppress,
		Now:       func() time.Time { return now },
		OnQuota:   func(session string) { *events = append(*events, session) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return obs, events, &now
}

func appendLog(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestObserverSkipsHistory(t *testing.T) {
	dir := t.TempDir()
	appendLog(t, filepath.Join(dir, "opencode.log"), quotaLine)
	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)

	obs.scan()
	if len(*events) != 0 {
		t.Fatalf("history replayed: %v", *events)
	}

	appendLog(t, filepath.Join(dir, "opencode.log"), quotaLine)
	obs.scan()
	if len(*events) != 1 || (*events)[0] != "ses_test123" {
		t.Fatalf("events = %v", *events)
	}
	stats := obs.Stats()
	if stats.Events != 1 || stats.Duplicates != 0 || stats.Errors != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestObserverIgnoresOtherErrors(t *testing.T) {
	dir := t.TempDir()
	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)
	obs.scan()

	appendLog(t, filepath.Join(dir, "opencode.log"), `level=ERROR message="Failed to drain Session" cause="AI.Error: auth failed" sessionID=ses_x`)
	appendLog(t, filepath.Join(dir, "opencode.log"), plainLine)
	obs.scan()
	if len(*events) != 0 {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverDeduplicatesSession(t *testing.T) {
	dir := t.TempDir()
	obs, events, now := newTestObserver(t, dir, 30*time.Second, nil)
	obs.scan()
	path := filepath.Join(dir, "opencode.log")

	appendLog(t, path, quotaLine)
	obs.scan()
	appendLog(t, path, quotaLine)
	obs.scan()
	if len(*events) != 1 {
		t.Fatalf("events = %v", *events)
	}
	if obs.Stats().Duplicates != 1 {
		t.Fatalf("stats = %+v", obs.Stats())
	}

	*now = now.Add(31 * time.Second)
	appendLog(t, path, quotaLine)
	obs.scan()
	if len(*events) != 2 {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverSuppressedByPlugin(t *testing.T) {
	dir := t.TempDir()
	suppress := true
	obs, events, _ := newTestObserver(t, dir, 30*time.Second, func() bool { return suppress })
	obs.scan()
	path := filepath.Join(dir, "opencode.log")

	appendLog(t, path, quotaLine)
	obs.scan()
	if len(*events) != 0 || obs.Stats().Suppressed != 1 {
		t.Fatalf("events=%v stats=%+v", *events, obs.Stats())
	}

	suppress = false
	appendLog(t, path, quotaLine)
	obs.scan()
	if len(*events) != 1 {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverResumesOffsetAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.log")
	obs, _, _ := newTestObserver(t, dir, time.Minute, nil)
	obs.scan()

	appendLog(t, path, quotaLine)
	obs.scan()

	events := &[]string{}
	restarted, err := New(Options{
		Path:      path,
		StatePath: filepath.Join(dir, "observer.json"),
		Dedup:     time.Minute,
		OnQuota:   func(session string) { *events = append(*events, session) },
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted.scan()
	if len(*events) != 0 {
		t.Fatalf("offset replay: %v", *events)
	}
	appendLog(t, path, quotaLine)
	restarted.scan()
	if len(*events) != 1 {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverResetsAfterTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.log")
	appendLog(t, path, quotaLine)
	appendLog(t, path, quotaLine)
	appendLog(t, path, quotaLine)
	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)
	obs.scan()

	// Replace the file with shorter content that contains one new error.
	if err := os.WriteFile(path, []byte(quotaLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	obs.scan()
	if len(*events) != 1 || obs.Stats().Offset != int64(len(quotaLine)+1) {
		t.Fatalf("events=%v stats=%+v", *events, obs.Stats())
	}
}

func TestObserverLineWithoutSession(t *testing.T) {
	dir := t.TempDir()
	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)
	obs.scan()

	appendLog(t, filepath.Join(dir, "opencode.log"), `level=ERROR message="Failed to drain Session" cause="AI.Error: Free usage limit reached"`)
	obs.scan()
	if len(*events) != 1 || (*events)[0] != "" {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverReadsFromZeroWhenLogAppears(t *testing.T) {
	dir := t.TempDir()
	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)

	if err := obs.ScanOnce(); err != nil {
		t.Fatalf("missing log must not fail: %v", err)
	}
	if obs.Stats().Errors != 0 {
		t.Fatalf("stats = %+v", obs.Stats())
	}

	appendLog(t, filepath.Join(dir, "opencode.log"), quotaLine)
	if err := obs.ScanOnce(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || (*events)[0] != "ses_test123" {
		t.Fatalf("events = %v", *events)
	}
}

func TestObserverDropsStaleOffsetWhenLogIsGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.log")
	raw, err := json.Marshal(persisted{Version: stateVersion, Path: path, Offset: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "observer.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	obs, events, _ := newTestObserver(t, dir, time.Minute, nil)
	if obs.Stats().Offset != 0 {
		t.Fatalf("stale offset kept: %+v", obs.Stats())
	}
	if err := obs.ScanOnce(); err != nil {
		t.Fatal(err)
	}

	appendLog(t, path, quotaLine)
	if err := obs.ScanOnce(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 {
		t.Fatalf("events = %v", *events)
	}
}
