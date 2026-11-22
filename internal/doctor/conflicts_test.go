package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBypassConfigDetectAndDisable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "infinite.json")
	raw := `{"mode":"WARP","auto":true,"country":"US","chain":"TOR,PROXY"}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	paths := []string{cfgPath}
	if !hasActiveBypassConfig(paths) {
		t.Fatal("expected active bypass config to be detected")
	}

	if !disableActiveBypassConfigs(paths) {
		t.Fatal("expected disableActiveBypassConfigs to report change")
	}

	if hasActiveBypassConfig(paths) {
		t.Fatal("expected bypass config to be inactive after disable")
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["mode"] != "NONE" {
		t.Fatalf("mode = %v, want NONE", doc["mode"])
	}
	if doc["auto"] != false {
		t.Fatalf("auto = %v, want false", doc["auto"])
	}
	if doc["country"] != "US" || doc["chain"] != "TOR,PROXY" {
		t.Fatalf("expected unrelated user keys preserved, got %+v", doc)
	}
}

func TestBypassConfigInactiveNotFlagged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "infinite.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mode":"NONE","auto":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasActiveBypassConfig([]string{cfgPath}) {
		t.Fatal("inactive config should not be flagged as conflict")
	}
}

func TestHijackedServiceProxyDetection(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Join(dir, "service.json")
	expected := "http://127.0.0.1:42987"

	cases := []struct {
		name     string
		proxyVal string
		want     bool
	}{
		{"own proxy", "http://127.0.0.1:42987", false},
		{"corporate proxy", "http://proxy.corp.local:8080", false},
		{"warp 40000", "http://127.0.0.1:40000", true},
		{"bypass 41987", "http://127.0.0.1:41987", true},
		{"infinite gate 18120", "http://127.0.0.1:18120", true},
		{"infinite tor 18118", "http://127.0.0.1:18118", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{
				"env": map[string]any{
					"HTTPS_PROXY": tc.proxyVal,
				},
			}
			b, _ := json.Marshal(doc)
			if err := os.WriteFile(svc, b, 0o600); err != nil {
				t.Fatal(err)
			}
			got := hasHijackedServiceProxyInFiles([]string{svc}, expected)
			if got != tc.want {
				t.Fatalf("proxy=%q: got %v, want %v", tc.proxyVal, got, tc.want)
			}
		})
	}
}

func TestIsWarpProxySettings(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "warp proxy mode",
			output: "(user set)\tMode: WarpProxy on port 40000\n(consumer overrides)\tWARP tunnel protocol: MASQUE\n",
			want:   true,
		},
		{
			name:   "full warp vpn mode",
			output: "(default)\tMode: Warp\n(consumer overrides)\tWARP tunnel protocol: MASQUE\n",
			want:   false,
		},
		{
			name:   "warp with doh mode",
			output: "(user set)\tMode: WarpWithDnsOverHttps\n",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWarpProxySettings(tc.output); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
