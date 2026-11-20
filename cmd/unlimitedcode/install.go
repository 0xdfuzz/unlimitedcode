package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/opencode"
	"unlimitedcode/internal/setup"
	"unlimitedcode/plugin"
)

// cmdInstall performs a zero-config plugin-first installation:
//   1. Copies the UnlimitedCode binary to the state home (so the plugin can launch it on demand).
//   2. Writes default config.json if missing.
//   3. Installs the embedded OpenCode plugin (which auto-starts UnlimitedCode with --parent-pid).
//   4. Stores HTTPS_PROXY/HTTP_PROXY/NO_PROXY in the OpenCode service env.
func cmdInstall(args []string, interactive bool) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	autostart := fs.Bool("autostart", false, "also install OS logon autostart (optional; plugin starts UnlimitedCode on demand by default)")
	fs.Parse(args)

	if interactive {
		if isAlreadyInstalled() {
			fmt.Printf("UnlimitedCode v%s (installed)\n\n", version)
			fmt.Println("  [1] Repair / Reinstall (update binary, plugin & proxy)")
			fmt.Println("  [2] Status & Diagnostics")
			fmt.Println("  [3] Uninstall (remove everything)")
			fmt.Println("  [0] Exit")
			fmt.Print("\nChoose [1/2/3/0]: ")
			choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			fmt.Println()
			switch strings.TrimSpace(choice) {
			case "1":
				// continue into install/repair below
			case "2":
				cmdDoctor(nil)
				fmt.Println()
				if setup.Health(config.Default().Control) {
					cmdStatus(nil)
				}
				pauseIfWindows()
				return
			case "3":
				cmdUninstall([]string{"--purge"})
				pauseIfWindows()
				return
			default:
				return
			}
		} else {
			fmt.Printf("UnlimitedCode v%s\n\n", version)
			fmt.Println("  [1] Install (plugin + proxy for OpenCode)")
			fmt.Println("  [2] Diagnostics (check environment)")
			fmt.Println("  [0] Exit")
			fmt.Print("\nChoose [1/2/0]: ")
			choice, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			fmt.Println()
			switch strings.TrimSpace(choice) {
			case "1":
				// continue into install below
			case "2":
				cmdDoctor(nil)
				pauseIfWindows()
				return
			default:
				return
			}
		}
	}

	fail := func(err error) {
		fmt.Fprintf(os.Stderr, "UnlimitedCode: %v\n", err)
		if interactive {
			pauseIfWindows()
		}
		os.Exit(1)
	}

	cfg, err := o.load(fs)
	if err != nil {
		fail(err)
	}
	official, _ := opencode.Find()

	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	installedBin := setup.InstalledPath()
	if err := setup.CopyBinary(self, installedBin); err != nil {
		fail(fmt.Errorf("install binary: %w", err))
	}
	fmt.Printf("binary    %s\n", installedBin)

	cfgPath, created, err := setup.EnsureConfig(cfg.Path)
	if err != nil {
		fail(err)
	}
	state := "existing"
	if created {
		state = "created"
	}
	fmt.Printf("config    %s (%s)\n", cfgPath, state)

	pluginPath, err := plugin.Install("")
	if err != nil {
		fail(err)
	}
	fmt.Printf("plugin    %s\n", pluginPath)

	proxyURL := "http://" + cfg.Listen
	if _, err := setup.SetOpenCodeServiceEnv(proxyURL); err != nil {
		fail(fmt.Errorf("configure OpenCode service env: %w", err))
	}
	if official != "" {
		for _, pair := range [][2]string{
			{"HTTPS_PROXY", proxyURL},
			{"HTTP_PROXY", proxyURL},
			{"NO_PROXY", "localhost,127.0.0.1,::1"},
		} {
			_ = exec.Command(official, "service", "set", "env", pair[0], pair[1]).Run()
		}
	}
	fmt.Printf("proxy     %s (OpenCode service env)\n", proxyURL)

	if *autostart {
		vbs, err := setup.InstallAutostart(self)
		if err != nil {
			fail(err)
		}
		fmt.Printf("autostart %s\n", vbs)
	}

	if warmDaemon(installedBin, cfg.Control) {
		fmt.Printf("daemon    http://%s (online & warm)\n", cfg.Listen)
	} else {
		fmt.Printf("daemon    will auto-start when OpenCode opens\n")
	}

	// Restart OpenCode background service if reachable so the new plugin & proxy apply immediately.
	if official != "" {
		if err := exec.CommandContext(context.Background(), official, "service", "restart").Run(); err == nil {
			fmt.Println("opencode  service restarted")
		}
	}

	fmt.Println()
	fmt.Println("Installed successfully! Restart OpenCode and enjoy :)")
	fmt.Println("Inside OpenCode, try: /unlimited · /unlimited-stats · /rotate")
	if interactive {
		pauseIfWindows()
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
	_, err = os.Stat(dir + string(os.PathSeparator) + plugin.Filename)
	return err == nil
}

func pauseIfWindows() {
	if runtime.GOOS == "windows" {
		fmt.Print("\nPress Enter to close...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

// warmDaemon starts the installed daemon in the background during installation
// so control.token and the MASQUE pool slots are registered and ready before
// the user opens OpenCode CLI or Desktop.
func warmDaemon(binPath, controlAddr string) bool {
	if setup.Health(controlAddr) {
		return true
	}
	cmd := exec.Command(binPath, "daemon")
	cmd.Env = cleanProxyEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		return false
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if setup.Health(controlAddr) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func cleanProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete UnlimitedCode state directory (config, logs, MASQUE slot keys)")
	fs.Parse(args)

	if err := plugin.Uninstall(""); err != nil {
		fmt.Fprintf(os.Stderr, "warning: remove plugin: %v\n", err)
	} else {
		fmt.Println("plugin    removed")
	}

	if vbs, err := setup.RemoveAutostart(); err == nil && vbs != "" {
		fmt.Printf("autostart removed %s\n", vbs)
	}

	_, _ = setup.ClearOpenCodeServiceEnv()
	if official, err := opencode.Find(); err == nil {
		for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"} {
			_ = exec.Command(official, "service", "unset", "env", key).Run()
		}
		_ = exec.Command(official, "service", "restart").Run()
	}
	fmt.Println("proxy     cleared from OpenCode service env")

	if *purge {
		home := config.Home()
		if err := os.RemoveAll(home); err != nil {
			fmt.Fprintf(os.Stderr, "warning: remove %s: %v\n", home, err)
		} else {
			fmt.Printf("state     removed %s\n", home)
		}
	}

	fmt.Println("UnlimitedCode uninstalled.")
}
