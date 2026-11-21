package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/control"
	"unlimitedcode/internal/doctor"
	"unlimitedcode/internal/rotate"
)

func controlRequest(method, addr, path string, body io.Reader, timeout time.Duration) []byte {
	token, err := control.ReadToken(config.TokenPath())
	if err != nil {
		fatal(fmt.Errorf("read control token: %w (is the daemon running?)", err))
	}
	req, err := http.NewRequest(method, control.URL(addr)+path, body)
	if err != nil {
		fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		fatal(fmt.Errorf("contact daemon on %s: %w", addr, err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		fatal(fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(data))))
	}
	return data
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	asJSON := fs.Bool("json", false, "print raw status JSON")
	fs.Parse(args)

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	body := controlRequest(http.MethodGet, cfg.Control, "/status", nil, 5*time.Second)

	if *asJSON {
		var pretty any
		if err := json.Unmarshal(body, &pretty); err != nil {
			fatal(err)
		}
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
		return
	}

	var status control.Status
	if err := json.Unmarshal(body, &status); err != nil {
		fatal(err)
	}
	uptime := (time.Duration(status.UptimeSeconds) * time.Second).Round(time.Second)
	fmt.Printf("UnlimitedCode %s  up %s\n", status.Version, uptime)
	fmt.Printf("proxy    %s\n", control.URL(status.Listen))
	fmt.Printf("control  %s\n", control.URL(status.Control))
	fmt.Printf("egress   %s -> %s\n", status.Egress.Mode, status.Egress.Upstream)
	if status.Pool != nil {
		fmt.Printf("pool     size=%d active=slot-%d ip=%s burnt=%d\n",
			status.Pool.Size, status.Pool.ActiveSlot, status.Pool.ActiveIP, status.Pool.BurntCount)
		for _, sl := range status.Pool.Slots {
			mark := " "
			if sl.Active {
				mark = "*"
			}
			fmt.Printf("         %s slot-%d %-9s ip=%s colo=%s recycles=%d\n",
				mark, sl.ID, sl.State, sl.IP, sl.Colo, sl.Recycles)
		}
	}
	fmt.Printf("tunnels  active=%d total=%d denied=%d failed=%d\n",
		status.Proxy.Active, status.Proxy.Total, status.Proxy.Denied, status.Proxy.Failed)
	fmt.Printf("bytes    up=%s down=%s\n", humanBytes(status.Proxy.BytesUp), humanBytes(status.Proxy.BytesDown))
	if status.HopEnabled {
		detail := fmt.Sprintf("count=%d failed=%d coalesced=%d limited=%d",
			status.Hop.Count, status.Hop.Failures, status.Hop.Coalesced, status.Hop.Limited)
		if status.Hop.LastAt != nil {
			detail += fmt.Sprintf(" last=%s %s %dms",
				status.Hop.LastAt.Format("15:04:05"),
				status.Hop.LastStatus,
				status.Hop.LastDurationMS)
		}
		fmt.Printf("rotate   %s\n", detail)
	} else {
		fmt.Printf("rotate   disabled\n")
	}
	if status.Proxy.LastEvent != "" {
		fmt.Printf("last     %s\n", status.Proxy.LastEvent)
	}
}

func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	fs.Parse(args)

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks := doctor.Run(ctx, cfg, version)
	failed := false
	for _, check := range checks {
		fmt.Printf("[%-4s] %-9s %s\n", check.Level, check.Name, check.Detail)
		if check.Level == doctor.Fail {
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func cmdRotate(args []string) {
	fs := flag.NewFlagSet("rotate", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	asJSON := fs.Bool("json", false, "print the raw result")
	fs.Parse(args)

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	timeout := time.Duration(cfg.Rotate.TimeoutSeconds+15) * time.Second
	body := controlRequest(http.MethodPost, cfg.Control, "/rotate", strings.NewReader("{}"), timeout)
	if *asJSON {
		fmt.Println(strings.TrimSpace(string(body)))
		return
	}
	var result rotate.Result
	if err := json.Unmarshal(body, &result); err != nil {
		fatal(err)
	}
	if result.Status == "ok" {
		fmt.Printf("rotate ok in %dms\n", result.DurationMS)
		return
	}
	if result.Error != "" {
		fatal(fmt.Errorf("rotate %s: %s", result.Status, result.Error))
	}
	fatal(fmt.Errorf("rotate %s", result.Status))
}
