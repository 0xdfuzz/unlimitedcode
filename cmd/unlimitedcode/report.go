package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/control"
	"unlimitedcode/internal/doctor"
)

var (
	reWinUser  = regexp.MustCompile(`(?i)[A-Z]:\\Users\\[^\\/\s]+`)
	reUnixHome = regexp.MustCompile(`/(home|Users)/[^/\s]+`)
	reRegID    = regexp.MustCompile(`/reg/[0-9a-fA-F-]{8,}`)
	reBearer   = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._-]+`)
)

func redactLine(s string) string {
	s = reWinUser.ReplaceAllString(s, "~")
	s = reUnixHome.ReplaceAllString(s, "~")
	s = reRegID.ReplaceAllString(s, "/reg/<redacted>")
	s = reBearer.ReplaceAllString(s, "Bearer <redacted>")
	return s
}

func fetchStatusSafe(controlAddr string) *control.Status {
	token, err := control.ReadToken(config.TokenPath())
	if err != nil || token == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, control.URL(controlAddr)+"/status", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return nil
	}
	var st control.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

func tailRedactedLog(path string, maxLines int) []string {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\r\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			out = append(out, redactLine(l))
		}
	}
	return out
}

// cmdReport prints a privacy-scrubbed diagnostic snapshot safe to paste into
// GitHub issues or Discord (usernames, paths, device IDs, and tokens redacted).
func cmdReport(_ []string) {
	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}

	fmt.Println("```text")
	fmt.Printf("UnlimitedCode Report | v%s | %s/%s | %s\n",
		version, runtime.GOOS, runtime.GOARCH, time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Println(strings.Repeat("-", 60))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	checks := doctor.Run(ctx, cfg, version)
	cancel()
	for _, c := range checks {
		fmt.Printf("[%-4s] %-9s %s\n", c.Level, c.Name, redactLine(c.Detail))
	}

	fmt.Println(strings.Repeat("-", 60))
	if st := fetchStatusSafe(cfg.Control); st != nil {
		up := (time.Duration(st.UptimeSeconds) * time.Second).Round(time.Second)
		fmt.Printf("daemon   v%s up=%s listen=%s\n", st.Version, up, st.Listen)
		if st.Pool != nil {
			fmt.Printf("pool     size=%d active=slot-%d burnt=%d\n", st.Pool.Size, st.Pool.ActiveSlot, st.Pool.BurntCount)
			for _, sl := range st.Pool.Slots {
				errNote := ""
				if sl.LastError != "" {
					errNote = " err=" + redactLine(sl.LastError)
				}
				fmt.Printf("         slot-%d state=%-9s colo=%-3s recycles=%d prefix=%s%s\n",
					sl.ID, sl.State, sl.Colo, sl.Recycles, sl.Prefix, errNote)
			}
		}
		fmt.Printf("tunnels  active=%d total=%d denied=%d failed=%d up=%s down=%s\n",
			st.Proxy.Active, st.Proxy.Total, st.Proxy.Denied, st.Proxy.Failed,
			humanBytes(st.Proxy.BytesUp), humanBytes(st.Proxy.BytesDown))
		fmt.Printf("rotate   count=%d failed=%d coalesced=%d limited=%d last=%s (%dms)\n",
			st.Hop.Count, st.Hop.Failures, st.Hop.Coalesced, st.Hop.Limited,
			st.Hop.LastStatus, st.Hop.LastDurationMS)
	} else {
		fmt.Println("daemon   not running")
	}

	if lines := tailRedactedLog(config.LogPath(), 20); len(lines) > 0 {
		fmt.Println(strings.Repeat("-", 60))
		fmt.Println("recent log (redacted):")
		for _, l := range lines {
			fmt.Println(" ", l)
		}
	}
	fmt.Println("```")
}
