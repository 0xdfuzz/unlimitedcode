package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"unlimitedcode/internal/opencode"
	"unlimitedcode/internal/setup"
	"unlimitedcode/plugin"
)

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	o := &overrides{}
	addConfigFlag(fs, o)
	dry := fs.Bool("dry-run", false, "show what would change")
	takeover := fs.Bool("takeover", false, "remove openhop from the user PATH and logon")
	restore := fs.Bool("restore", false, "restore the PATH and logon value saved by --takeover")
	service := fs.Bool("service", false, "install logon autostart and start the daemon")
	proxyFlag := fs.Bool("proxy", false, "store the UnlimitedCode proxy in the OpenCode service env")
	fs.Parse(args)
	if *takeover && *restore {
		fatal(errors.New("--takeover and --restore cannot be combined"))
	}

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	official, _ := opencode.Find()
	if official != "" {
		label := official
		if ver, err := opencode.Version(context.Background(), official); err == nil && ver != "" {
			label += " (" + ver + ")"
		}
		fmt.Printf("official  %s\n", label)
	} else {
		fmt.Println("official  not on PATH (Desktop-only mode)")
	}

	if *dry {
		if _, err := os.Stat(cfg.Path); os.IsNotExist(err) {
			fmt.Printf("config    %s (would create)\n", cfg.Path)
		} else {
			fmt.Printf("config    %s\n", cfg.Path)
		}
		if dir, err := plugin.Dir(); err == nil {
			path := filepath.Join(dir, plugin.Filename)
			if _, err := os.Stat(path); os.IsNotExist(err) {
				fmt.Printf("plugin    %s (would install)\n", path)
			} else {
				fmt.Printf("plugin    %s\n", path)
			}
		}
	} else {
		path, created, err := setup.EnsureConfig(cfg.Path)
		if err != nil {
			fatal(err)
		}
		state := "existing"
		if created {
			state = "created"
		}
		fmt.Printf("config    %s (%s)\n", path, state)
		path, err = plugin.Install("")
		if err != nil {
			fatal(err)
		}
		fmt.Printf("plugin    %s (installed)\n", path)
	}

	if setup.Health(cfg.Control) {
		fmt.Println("daemon    running")
	} else {
		fmt.Println("daemon    not running")
	}

	visible := opencode.First()
	switch {
	case visible == "":
		fmt.Println("path      no opencode launcher on PATH")
	case opencode.IsOpenhopShim(visible):
		fmt.Printf("path      %s (openhop shim; --takeover removes it)\n", visible)
	default:
		fmt.Printf("path      %s\n", visible)
	}

	if *takeover {
		if *dry {
			removed, run, err := setup.TakeoverPlan()
			if err != nil {
				fatal(err)
			}
			printRemoved(removed)
			if run != "" {
				fmt.Printf("startup   would remove %s\n", run)
			}
			if len(removed) == 0 && run == "" {
				fmt.Println("path      openhop not present")
			}
		} else {
			removed, added, runRemoved, err := setup.TakeoverPath(official)
			if err != nil {
				fatal(err)
			}
			printRemoved(removed)
			for _, dir := range added {
				fmt.Printf("path      added %s\n", dir)
			}
			if runRemoved {
				fmt.Println("startup   removed openhop logon entry")
			}
			if len(removed) == 0 && !runRemoved {
				fmt.Println("path      openhop not present")
			}
			if len(removed) > 0 || runRemoved {
				fmt.Println("path      log out or open a new terminal for the change to apply")
			}
		}
	}
	if *restore {
		if *dry {
			fmt.Println("path      would restore the saved PATH and logon value")
		} else {
			pathRestored, runRestored, err := setup.Restore()
			if err != nil {
				fatal(err)
			}
			fmt.Printf("path      restored path=%v startup=%v\n", pathRestored, runRestored)
		}
	}
	if *service {
		if *dry {
			fmt.Printf("service   would install autostart and start %s\n", setup.InstalledPath())
		} else {
			self, err := os.Executable()
			if err != nil {
				fatal(err)
			}
			vbs, err := setup.InstallAutostart(self)
			if err != nil {
				fatal(err)
			}
			fmt.Printf("service   %s\n", vbs)
			if !setup.Health(cfg.Control) {
				if err := setup.StartDetached(setup.InstalledPath()); err != nil {
					fatal(err)
				}
				if err := setup.WaitHealthy(cfg.Control, 20*time.Second); err != nil {
					fatal(err)
				}
				fmt.Println("daemon    started")
			}
		}
	}
	if *proxyFlag {
		proxyURL := "http://" + cfg.Listen
		if *dry {
			fmt.Printf("proxy     would set HTTPS_PROXY/HTTP_PROXY=%s in OpenCode service config\n", proxyURL)
		} else {
			if _, err := setup.SetOpenCodeServiceEnv(proxyURL); err != nil {
				fatal(fmt.Errorf("configure OpenCode service env: %w", err))
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
			fmt.Println("proxy     stored; restart OpenCode to apply")
		}
	}
	if !*dry {
		fmt.Println("next      unlimitedcode doctor")
	}
}

func printRemoved(removed []string) {
	for _, entry := range removed {
		fmt.Printf("path      removed %s\n", entry)
	}
}
