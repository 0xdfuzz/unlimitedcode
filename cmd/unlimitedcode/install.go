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

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	official, err := opencode.Find()
	if err != nil {
		fatal(fmt.Errorf("OpenCode not found: %w (install OpenCode first)", err))
	}

	self, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	installedBin := setup.InstalledPath()
	if err := setup.CopyBinary(self, installedBin); err != nil {
		fatal(fmt.Errorf("install binary: %w", err))
	}
	fmt.Printf("binary    %s\n", installedBin)

	cfgPath, created, err := setup.EnsureConfig(cfg.Path)
	if err != nil {
		fatal(err)
	}
	state := "existing"
	if created {
		state = "created"
	}
	fmt.Printf("config    %s (%s)\n", cfgPath, state)

	pluginPath, err := plugin.Install("")
	if err != nil {
		fatal(err)
	}
	fmt.Printf("plugin    %s\n", pluginPath)

	proxyURL := "http://" + cfg.Listen
	for _, pair := range [][2]string{
		{"HTTPS_PROXY", proxyURL},
		{"HTTP_PROXY", proxyURL},
		{"NO_PROXY", "localhost,127.0.0.1,::1"},
	} {
		if out, err := exec.Command(official, "service", "set", "env", pair[0], pair[1]).CombinedOutput(); err != nil {
			fatal(fmt.Errorf("service set env %s: %v: %s", pair[0], err, strings.TrimSpace(string(out))))
		}
	}
	fmt.Printf("proxy     %s (OpenCode service env)\n", proxyURL)

	if *autostart {
		vbs, err := setup.InstallAutostart(self)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("autostart %s\n", vbs)
	}

	// Restart OpenCode background service if reachable so the new plugin & proxy apply immediately.
	if err := exec.CommandContext(context.Background(), official, "service", "restart").Run(); err == nil {
		fmt.Println("opencode  service restarted")
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

	if official, err := opencode.Find(); err == nil {
		for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"} {
			_ = exec.Command(official, "service", "unset", "env", key).Run()
		}
		fmt.Println("proxy     cleared from OpenCode service env")
		_ = exec.Command(official, "service", "restart").Run()
	}

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
