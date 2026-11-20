// Package warp drives the local Cloudflare WARP client through warp-cli.
package warp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const windowsDefault = `C:\Program Files\Cloudflare\Cloudflare WARP\warp-cli.exe`

// Runner executes warp-cli with the given arguments.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type execRunner struct {
	path string
}

func (r execRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.path, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return out, fmt.Errorf("%s %s: %w: %s", filepath.Base(r.path), strings.Join(args, " "), err, detail)
		}
		return out, fmt.Errorf("%s %s: %w", filepath.Base(r.path), strings.Join(args, " "), err)
	}
	return out, nil
}

type CLI struct {
	Path string
	Run  Runner
	Poll time.Duration
}

func New(path string) *CLI {
	return &CLI{Path: path, Run: execRunner{path: path}, Poll: 200 * time.Millisecond}
}

func (c *CLI) Name() string { return "warp-cli" }

// Find resolves warp-cli. An explicit path wins, then PATH, then the standard
// Windows install location.
func Find(explicit string) (string, error) {
	candidates := []string{}
	if strings.TrimSpace(explicit) != "" {
		candidates = append(candidates, strings.TrimSpace(explicit))
	} else {
		if runtime.GOOS == "windows" {
			candidates = append(candidates, windowsDefault)
		}
		candidates = append(candidates, "warp-cli")
		if runtime.GOOS == "windows" {
			candidates = append(candidates, "warp-cli.exe")
		}
	}
	for _, candidate := range candidates {
		if strings.ContainsAny(candidate, `/\`) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
			continue
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("warp-cli not found; set hop.warp_cli")
}

func (c *CLI) statusArgs() []string {
	return []string{"--no-ansi", "-j", "status"}
}

func (c *CLI) Status(ctx context.Context) (string, error) {
	out, err := c.run(ctx, c.statusArgs()...)
	if err == nil {
		if start := strings.Index(string(out), "{"); start >= 0 {
			var payload struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(out[start:], &payload) == nil && payload.Status != "" {
				return payload.Status, nil
			}
		}
	}

	out, err = c.run(ctx, "--no-ansi", "status")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"Status update:", "Status:"} {
			if strings.HasPrefix(line, prefix) {
				if status := strings.TrimSpace(strings.TrimPrefix(line, prefix)); status != "" {
					return status, nil
				}
			}
		}
	}
	return "", fmt.Errorf("warp-cli status: no status in output %q", strings.TrimSpace(string(out)))
}

func (c *CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if c.Run == nil {
		return nil, errors.New("warp-cli runner is not configured")
	}
	return c.Run.Run(ctx, args...)
}

// Reconnect disconnects the current tunnel, reconnects, and waits for the
// client to report Connected. It does not inspect the public IP.
func (c *CLI) Reconnect(ctx context.Context) error {
	status, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("warp status: %w", err)
	}

	if !strings.EqualFold(status, "Disconnected") {
		if _, err := c.run(ctx, "--no-ansi", "disconnect"); err != nil {
			// A race with an already-disconnected client is not a hop failure.
			if current, statusErr := c.Status(ctx); statusErr != nil || !strings.EqualFold(current, "Disconnected") {
				return fmt.Errorf("warp disconnect: %w", err)
			}
		}
		if err := c.waitFor(ctx, "Disconnected"); err != nil {
			return err
		}
	}

	// Rotate cryptographic keys and reset endpoint before connecting so WARP
	// assigns a fresh exit IP/subnet instead of reusing the existing session.
	_, _ = c.run(ctx, "--no-ansi", "tunnel", "rotate-keys")
	_, _ = c.run(ctx, "--no-ansi", "tunnel", "endpoint", "reset")

	if _, err := c.run(ctx, "--no-ansi", "connect"); err != nil {
		return fmt.Errorf("warp connect: %w", err)
	}
	if err := c.waitFor(ctx, "Connected"); err != nil {
		return err
	}
	return nil
}

func (c *CLI) waitFor(ctx context.Context, want string) error {
	poll := c.Poll
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	last := ""
	lastErr := error(nil)
	for {
		status, err := c.Status(ctx)
		if err != nil {
			lastErr = err
		} else {
			last = status
			lastErr = nil
			if strings.EqualFold(status, want) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("warp did not reach %s (last %s, last error %v): %w", want, last, lastErr, ctx.Err())
			}
			return fmt.Errorf("warp did not reach %s (last %s): %w", want, last, ctx.Err())
		case <-time.After(poll):
		}
	}
}
