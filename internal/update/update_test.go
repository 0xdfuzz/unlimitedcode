package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, cand string
		want      bool
	}{
		{"0.3.1", "0.3.2", true},
		{"v0.3.1", "v0.4.0", true},
		{"0.3.2", "0.3.2", false},
		{"0.3.2-dev", "0.3.2", false},
		{"0.3.2-dev", "0.3.3", true},
		{"1.0.0", "0.9.9", false},
		{"invalid", "0.3.2", false},
	}
	for _, tc := range cases {
		if got := IsNewer(tc.cur, tc.cand); got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.cur, tc.cand, got, tc.want)
		}
	}
}

func TestCheckAndDownloadVerify(t *testing.T) {
	payload := []byte("fake-binary-bytes-v0.3.2")
	sumBytes := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sumBytes[:])
	assetName := ExpectedAsset("0.3.2", "linux", "amd64")

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": "v0.3.2",
				"html_url": "https://github.com/0xdfuzz/unlimitedcode/releases/tag/v0.3.2",
				"assets": []map[string]string{
					{"name": assetName, "browser_download_url": srv.URL + "/bin"},
					{"name": "checksums.txt", "browser_download_url": srv.URL + "/checksums.txt"},
				},
			})
		case "/bin":
			_, _ = w.Write(payload)
		case "/checksums.txt":
			_, _ = w.Write([]byte(sumHex + "  " + assetName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rel, err := checkFrom(context.Background(), srv.URL, Repo, "0.3.1", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if rel == nil || rel.Version != "0.3.2" {
		t.Fatalf("rel = %+v", rel)
	}

	gotPath, err := DownloadAndVerify(context.Background(), rel, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded = %q, want %q", got, payload)
	}

	// Up-to-date version returns nil release.
	upToDate, err := checkFrom(context.Background(), srv.URL, Repo, "0.3.2", "linux", "amd64")
	if err != nil || upToDate != nil {
		t.Fatalf("upToDate = %+v, err = %v", upToDate, err)
	}
}
