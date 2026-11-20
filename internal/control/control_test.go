package control_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kestrel/internal/control"
	"kestrel/internal/hop"
	"kestrel/internal/proxy"
)

const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type fakeHopper struct {
	mu     sync.Mutex
	reqs   []hop.Request
	result hop.Result
}

func (f *fakeHopper) Hop(ctx context.Context, req hop.Request) hop.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.result.Status == "" {
		return hop.Result{Status: "ok", DurationMS: 12}
	}
	return f.result
}

func (f *fakeHopper) Stats() hop.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return hop.Stats{Enabled: true, Count: int64(len(f.reqs)), LastStatus: "ok"}
}

func newServerWithHop(t *testing.T, hopper control.Hopper) *control.Server {
	t.Helper()
	srv, err := control.New(control.Options{
		Listen:    "127.0.0.1:0",
		Token:     token,
		Version:   "test",
		StartedAt: time.Now(),
		Egress:    control.Egress{Mode: "warp", Upstream: "http://127.0.0.1:40000"},
		Stats:     func() proxy.Stats { return proxy.Stats{Total: 3} },
		Hop:       hopper,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func newServer(t *testing.T) *control.Server {
	t.Helper()
	srv, err := control.New(control.Options{
		Listen:    "127.0.0.1:0",
		Token:     token,
		Version:   "test",
		StartedAt: time.Now(),
		Egress:    control.Egress{Mode: "warp", Upstream: "http://127.0.0.1:40000"},
		Stats:     func() proxy.Stats { return proxy.Stats{Total: 3} },
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestHealthNeedsNoToken(t *testing.T) {
	ts := httptest.NewServer(newServer(t).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("body = %s", body)
	}
}

func TestStatusRequiresToken(t *testing.T) {
	ts := httptest.NewServer(newServer(t).Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"total":3`) {
		t.Fatalf("body = %s", body)
	}
}

func TestEventTriggersHop(t *testing.T) {
	fake := &fakeHopper{}
	ts := httptest.NewServer(newServerWithHop(t, fake).Handler())
	defer ts.Close()

	body := bytes.NewBufferString(`{"type":"provider.free-limit","session":"ses_1","model":"opencode/test","agent":"build","attempt":2}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/event", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var result hop.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.reqs) != 1 {
		t.Fatalf("requests = %+v", fake.reqs)
	}
	got := fake.reqs[0]
	if got.Session != "ses_1" || got.Model != "opencode/test" || got.Attempt != 2 || got.Reason != "provider.free-limit" {
		t.Fatalf("request = %+v", got)
	}
}

func TestEventIgnoresOtherTypes(t *testing.T) {
	fake := &fakeHopper{}
	ts := httptest.NewServer(newServerWithHop(t, fake).Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/event", bytes.NewBufferString(`{"type":"provider.auth"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result hop.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ignored" {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.reqs) != 0 {
		t.Fatalf("requests = %+v", fake.reqs)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.token")
	first, err := control.EnsureToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d", len(first))
	}
	second, err := control.ReadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("token mismatch")
	}
}

func postJSON(t *testing.T, url, body string, authorized bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEventFreeLimitTriggersHop(t *testing.T) {
	hopper := &fakeHopper{}
	ts := httptest.NewServer(newServerWithHop(t, hopper).Handler())
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/event", `{"type":"provider.free-limit","session":"ses_1","model":"opencode/free","agent":"build","attempt":2}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var result hop.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("result = %+v", result)
	}

	hopper.mu.Lock()
	defer hopper.mu.Unlock()
	if len(hopper.reqs) != 1 {
		t.Fatalf("requests = %d", len(hopper.reqs))
	}
	req := hopper.reqs[0]
	if req.Session != "ses_1" || req.Model != "opencode/free" || req.Attempt != 2 || req.Reason != "provider.free-limit" {
		t.Fatalf("request = %+v", req)
	}
}

func TestEventOtherTypeIgnored(t *testing.T) {
	hopper := &fakeHopper{}
	ts := httptest.NewServer(newServerWithHop(t, hopper).Handler())
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/event", `{"type":"other","attempt":2}`, true)
	defer resp.Body.Close()
	var result hop.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ignored" {
		t.Fatalf("result = %+v", result)
	}
	hopper.mu.Lock()
	defer hopper.mu.Unlock()
	if len(hopper.reqs) != 0 {
		t.Fatalf("requests = %d", len(hopper.reqs))
	}
}

func TestHopRequiresToken(t *testing.T) {
	ts := httptest.NewServer(newServerWithHop(t, &fakeHopper{}).Handler())
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/hop", `{}`, false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestStatusIncludesHop(t *testing.T) {
	hopper := &fakeHopper{}
	hopper.result = hop.Result{Status: "ok"}
	ts := httptest.NewServer(newServerWithHop(t, hopper).Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"hop_enabled":true`) {
		t.Fatalf("body = %s", body)
	}
}
