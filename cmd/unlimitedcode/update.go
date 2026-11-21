package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/setup"
	"unlimitedcode/internal/update"
)

func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	yes := fs.Bool("yes", false, "install update without prompting")
	fs.BoolVar(yes, "y", false, "install update without prompting")
	checkOnly := fs.Bool("check", false, "only check whether a newer release is available")
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	rel, err := update.Check(ctx, version)
	cancel()
	if err != nil {
		fatal(fmt.Errorf("check for updates: %w", err))
	}

	if rel == nil {
		fmt.Printf("Already up to date (v%s).\n", strings.TrimPrefix(version, "v"))
		return
	}

	fmt.Printf("Update available: v%s -> %s\n", strings.TrimPrefix(version, "v"), rel.Tag)
	if rel.HTMLURL != "" {
		fmt.Printf("Release: %s\n", rel.HTMLURL)
	}
	if *checkOnly {
		return
	}

	if !*yes {
		fmt.Print("Install update now? [Y/n]: ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "" && answer != "y" && answer != "yes" {
			fmt.Println("Update cancelled.")
			return
		}
	}

	if err := applyReleaseUpdate(rel); err != nil {
		fatal(err)
	}
}

func checkUpdateQuick() *update.Release {
	ctx, cancel := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer cancel()
	rel, err := update.Check(ctx, version)
	if err != nil {
		return nil
	}
	return rel
}

func applyReleaseUpdate(rel *update.Release) error {
	fmt.Printf("Downloading %s...\n", rel.AssetName)
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	tmpBin, err := update.DownloadAndVerify(dlCtx, rel, config.Home())
	dlCancel()
	if err != nil {
		return err
	}
	defer os.Remove(tmpBin)

	cfg, err := config.Load("")
	if err != nil {
		cfg = config.Default()
	}
	stopRunningDaemon(cfg.Control)

	dest := setup.InstalledPath()
	if err := setup.CopyBinary(tmpBin, dest); err != nil {
		return fmt.Errorf("install updated binary: %w", err)
	}

	// Run the newly installed binary's install step so the embedded OpenCode
	// plugin bundle (server, TUI, Desktop) stays in sync with the binary.
	installCmd := exec.Command(dest, "install")
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("refresh plugin after update: %w", err)
	}

	_ = os.Remove(filepath.Join(config.Home(), "update_skip"))
	fmt.Printf("\nUpdated to %s! Restart OpenCode to load the new plugin.\n", rel.Tag)
	return nil
}
