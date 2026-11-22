package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	Repo           = "0xdfuzz/unlimitedcode"
	defaultAPIBase = "https://api.github.com"
	maxBinaryBytes = 128 << 20
)

type Release struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	AssetName   string `json:"asset_name"`
	AssetURL    string `json:"asset_url"`
	ChecksumURL string `json:"checksum_url,omitempty"`
	HTMLURL     string `json:"html_url,omitempty"`
}

var directTransport = &http.Transport{
	Proxy:           nil,
	MaxIdleConns:    2,
	IdleConnTimeout: 10 * time.Second,
}

func directClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: directTransport,
	}
}

// ExpectedAsset returns the GitHub release filename for a target platform.
func ExpectedAsset(version, goos, goarch string) string {
	v := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(version), "v"), "V")
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("unlimitedcode_%s_%s_%s%s", v, goos, goarch, ext)
}

// IsNewer reports whether candidate is a strictly higher major.minor.patch
// release than current. Pre-release suffixes like "-dev" are stripped before
// comparing, so a local "0.3.2-dev" build is not prompted to downgrade/reinstall 0.3.2.
func IsNewer(current, candidate string) bool {
	c, ok1 := parseSemver(current)
	n, ok2 := parseSemver(candidate)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if n[i] != c[i] {
			return n[i] > c[i]
		}
	}
	return false
}

func parseSemver(raw string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return out, false
		}
		out[i] = v
	}
	return out, true
}

// Check queries GitHub Releases for a version newer than currentVersion.
// Returns (nil, nil) when already up to date.
func Check(ctx context.Context, currentVersion string) (*Release, error) {
	return checkFrom(ctx, defaultAPIBase, Repo, currentVersion, runtime.GOOS, runtime.GOARCH)
}

func checkFrom(ctx context.Context, apiBase, repo, currentVersion, goos, goarch string) (*Release, error) {
	url := strings.TrimRight(apiBase, "/") + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "unlimitedcode/"+currentVersion)

	resp, err := directClient(6 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github releases returned %s", resp.Status)
	}

	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512<<10)).Decode(&payload); err != nil {
		return nil, err
	}
	tag := strings.TrimSpace(payload.TagName)
	ver := strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	if !IsNewer(currentVersion, ver) {
		return nil, nil
	}

	wantAsset := ExpectedAsset(ver, goos, goarch)
	rel := &Release{
		Tag:       tag,
		Version:   ver,
		AssetName: wantAsset,
		HTMLURL:   payload.HTMLURL,
	}
	for _, a := range payload.Assets {
		switch a.Name {
		case wantAsset:
			rel.AssetURL = a.BrowserDownloadURL
		case "checksums.txt":
			rel.ChecksumURL = a.BrowserDownloadURL
		}
	}
	if rel.AssetURL == "" {
		return nil, fmt.Errorf("release %s does not include asset %s", tag, wantAsset)
	}
	return rel, nil
}

// DownloadAndVerify downloads the release binary into dir and verifies its
// SHA-256 digest against checksums.txt when published.
func DownloadAndVerify(ctx context.Context, rel *Release, dir string) (string, error) {
	if rel == nil || rel.AssetURL == "" {
		return "", errors.New("missing release asset URL")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "unlimitedcode-updater")

	resp, err := directClient(2 * time.Minute).Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rel.AssetName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %s", rel.AssetName, resp.Status)
	}

	pattern := ".update-*"
	if runtime.GOOS == "windows" {
		pattern = ".update-*.exe"
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func(err error) (string, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(resp.Body, maxBinaryBytes)); err != nil {
		return cleanup(fmt.Errorf("save %s: %w", rel.AssetName, err))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	gotSum := hex.EncodeToString(hasher.Sum(nil))

	if rel.ChecksumURL != "" {
		wantSum, err := fetchChecksum(ctx, rel.ChecksumURL, rel.AssetName)
		if err != nil {
			_ = os.Remove(tmpName)
			return "", err
		}
		if !strings.EqualFold(gotSum, wantSum) {
			_ = os.Remove(tmpName)
			return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", rel.AssetName, gotSum, wantSum)
		}
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

func fetchChecksum(ctx context.Context, checksumURL, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "unlimitedcode-updater")

	resp, err := directClient(15 * time.Second).Do(req)
	if err != nil {
		return "", fmt.Errorf("download checksums.txt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download checksums.txt: HTTP %s", resp.Status)
	}

	sc := bufio.NewScanner(io.LimitReader(resp.Body, 256<<10))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksums.txt missing entry for %s", assetName)
}
