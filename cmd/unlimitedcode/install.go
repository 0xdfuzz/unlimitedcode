package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/control"
	"unlimitedcode/internal/opencode"
	"unlimitedcode/internal/setup"
	"unlimitedcode/plugin"
)

// cmdInstall performs a zero-config plugin-first installation:
//  1. Copies the UnlimitedCode binary to the state home (so the plugin can launch it on demand).
//  2. Writes default config.json if missing (preserving installed_at across upgrades).
//  3. Installs the embedded OpenCode plugin bundle (server + TUI + Desktop).
//  4. Stores HTTPS_PROXY/HTTP_PROXY/NO_PROXY in OpenCode service config files.
func cmdInstall(args []string, interactive bool) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	fs.Parse(args)

	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "unlimitedcode: %v\n", err)
		if interactive {
			pauseIfWindows()
		}
		os.Exit(1)
	}

	if interactive {
		avail := checkUpdateQuick()
		if isAlreadyInstalled() {
			fmt.Printf("UnlimitedCode v%s (installed)\n", version)
			if avail != nil {
				fmt.Printf("Update available: %s\n\n", avail.Tag)
				fmt.Printf("  [1] Update to %s\n", avail.Tag)
				fmt.Println("  [2] Repair / Reinstall (current binary)")
				fmt.Println("  [3] Status & Diagnostics")
				fmt.Println("  [4] Uninstall (remove everything)")
				fmt.Println("  [0] Exit")
				fmt.Print("\nChoose [1/2/3/4/0]: ")
				choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				fmt.Println()
				switch strings.TrimSpace(choice) {
				case "1":
					if err := applyReleaseUpdate(avail); err != nil {
						fail(err)
					}
					pauseIfWindows()
					return
				case "2":
					// continue into install/repair below
				case "3":
					showInteractiveDiagnostics()
					pauseIfWindows()
					return
				case "4":
					cmdUninstall(nil)
					pauseIfWindows()
					return
				default:
					return
				}
			} else {
				fmt.Println()
				fmt.Println("  [1] Repair / Reinstall (update binary, plugin & proxy)")
				fmt.Println("  [2] Check for updates")
				fmt.Println("  [3] Status & Diagnostics")
				fmt.Println("  [4] Uninstall (remove everything)")
				fmt.Println("  [0] Exit")
				fmt.Print("\nChoose [1/2/3/4/0]: ")
				choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				fmt.Println()
				switch strings.TrimSpace(choice) {
				case "1":
					// continue into install/repair below
				case "2":
					cmdUpdate(nil)
					pauseIfWindows()
					return
				case "3":
					showInteractiveDiagnostics()
					pauseIfWindows()
					return
				case "4":
					cmdUninstall(nil)
					pauseIfWindows()
					return
				default:
					return
				}
			}
		} else {
			fmt.Printf("UnlimitedCode v%s\n", version)
			if avail != nil {
				fmt.Printf("Update available: %s\n\n", avail.Tag)
				fmt.Printf("  [1] Update to %s & Install (recommended)\n", avail.Tag)
				fmt.Printf("  [2] Install this version (v%s)\n", version)
				fmt.Println("  [3] Diagnostics (check environment)")
				fmt.Println("  [0] Exit")
				fmt.Print("\nChoose [1/2/3/0]: ")
				choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				fmt.Println()
				switch strings.TrimSpace(choice) {
				case "1":
					if err := applyReleaseUpdate(avail); err != nil {
						fail(err)
					}
					pauseIfWindows()
					return
				case "2":
					// continue into install below
				case "3":
					cmdDoctor(nil)
					pauseIfWindows()
					return
				default:
					return
				}
			} else {
				fmt.Println()
				fmt.Println("  [1] Install (plugin + proxy for OpenCode)")
				fmt.Println("  [2] Check for updates")
				fmt.Println("  [3] Diagnostics (check environment)")
				fmt.Println("  [0] Exit")
				fmt.Print("\nChoose [1/2/3/0]: ")
				choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				fmt.Println()
				switch strings.TrimSpace(choice) {
				case "1":
					// continue into install below
				case "2":
					cmdUpdate(nil)
					pauseIfWindows()
					return
				case "3":
					cmdDoctor(nil)
					pauseIfWindows()
					return
				default:
					return
				}
			}
		}
	}

	cfg, err := o.load(fs)
	if err != nil {
		fail(err)
	}

	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	installedBin := setup.InstalledPath()
	if err := setup.CopyBinary(self, installedBin); err != nil {
		fail(fmt.Errorf("install binary: %w", err))
	}
	fmt.Printf("binary    %s\n", installedBin)

	setup.EnsureInstalledAt(config.Home())

	pluginPath, err := plugin.Install("")
	if err != nil {
		fail(err)
	}
	fmt.Printf("plugin    %s\n", pluginPath)

	proxyURL := "http://" + cfg.Listen
	if _, err := setup.SetOpenCodeServiceEnv(proxyURL); err != nil {
		fail(fmt.Errorf("configure OpenCode service env: %w", err))
	}
	fmt.Printf("proxy     %s (OpenCode service env)\n", proxyURL)

	// Restart every discovered OpenCode background service (CLI + Desktop sidecar)
	// so the updated plugin and proxy env apply immediately.
	if restartOpenCodeServices() {
		fmt.Println("opencode  service restarted")
	}

	fmt.Println()
	fmt.Println("Installed! Restart OpenCode and you're good to go.")
	fmt.Println("Inside OpenCode, use /unlimited or /rotate.")
	if interactive {
		pauseIfWindows()
	}
}

func showInteractiveDiagnostics() {
	cmdDoctor(nil)
	fmt.Println()
	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}
	if setup.Health(cfg.Control) {
		cmdStatus(nil)
	}
}

func isAlreadyInstalled() bool {
	if _, err := os.Stat(setup.InstalledPath()); err != nil {
		return false
	}
	dir, err := plugin.Dir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, plugin.Filename))
	return err == nil
}

func pauseIfWindows() {
	if runtime.GOOS == "windows" {
		fmt.Print("\nPress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	keepState := fs.Bool("keep-state", false, "keep UnlimitedCode state directory (config, logs, MASQUE slot keys)")
	// Accepted as a no-op for backwards compatibility (state is purged by default).
	_ = fs.Bool("purge", true, "remove UnlimitedCode state directory (default)")
	fs.Parse(args)

	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}
	if stopRunningDaemon(cfg.Control) {
		fmt.Println("daemon    stopped")
	}

	if err := plugin.Uninstall(""); err != nil {
		fmt.Fprintf(os.Stderr, "warning: remove plugin: %v\n", err)
	} else {
		fmt.Println("plugin    removed")
	}

	if vbs, err := setup.RemoveLegacyAutostart(); err == nil && vbs != "" {
		fmt.Printf("autostart removed %s\n", vbs)
	}

	proxyURL := "http://" + cfg.Listen
	if cleared, _ := setup.ClearOpenCodeServiceEnv(proxyURL); len(cleared) > 0 {
		fmt.Println("proxy     cleared from OpenCode service env")
	}
	if restartOpenCodeServices() {
		fmt.Println("opencode  service restarted")
	}

	if !*keepState {
		home := config.Home()
		if err := os.RemoveAll(home); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove %s: %v\n", home, err)
		} else {
			fmt.Printf("state     removed %s\n", home)
		}
	}

	fmt.Println("UnlimitedCode uninstalled.")
}

func restartOpenCodeServices() bool {
	restarted := false
	for _, bin := range opencode.FindAll() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := exec.CommandContext(ctx, bin, "service", "restart").Run()
		cancel()
		if err == nil {
			restarted = true
		}
	}
	return restarted
}

func stopRunningDaemon(controlAddr string) bool {
	if !setup.Health(controlAddr) {
		return false
	}
	token, err := control.ReadToken(config.TokenPath())
	if err != nil || token == "" {
		return false
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+controlAddr+"/shutdown", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !setup.Health(controlAddr) {
			time.Sleep(150 * time.Millisecond)
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
