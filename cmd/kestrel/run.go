package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"kestrel/internal/opencode"
	"kestrel/plugin"
)

func cmdEnv(args []string) {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	o := &overrides{}
	addOverrides(fs, o)
	format := fs.String("format", "plain", "plain, powershell or json")
	fs.Parse(args)

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}
	proxyURL := "http://" + cfg.Listen
	noProxy := "localhost,127.0.0.1,::1"

	switch *format {
	case "plain":
		fmt.Printf("HTTPS_PROXY=%s\n", proxyURL)
		fmt.Printf("HTTP_PROXY=%s\n", proxyURL)
		fmt.Printf("NO_PROXY=%s\n", noProxy)
	case "powershell":
		fmt.Printf("$env:HTTPS_PROXY=%q\n", proxyURL)
		fmt.Printf("$env:HTTP_PROXY=%q\n", proxyURL)
		fmt.Printf("$env:NO_PROXY=%q\n", noProxy)
	case "json":
		out, _ := json.MarshalIndent(map[string]string{
			"HTTPS_PROXY": proxyURL,
			"HTTP_PROXY":  proxyURL,
			"NO_PROXY":    noProxy,
		}, "", "  ")
		fmt.Println(string(out))
	default:
		fatal(fmt.Errorf("unknown format %q", *format))
	}
}

func cmdPlugin(args []string) {
	if len(args) == 0 {
		fatal(fmt.Errorf("plugin: use install, uninstall or path"))
	}
	sub := args[0]
	fs := flag.NewFlagSet("plugin "+sub, flag.ExitOnError)
	dir := fs.String("dir", "", "OpenCode plugin directory")
	fs.Parse(args[1:])

	switch sub {
	case "install":
		path, err := plugin.Install(*dir)
		if err != nil {
			fatal(err)
		}
		fmt.Println(path)
	case "uninstall":
		if err := plugin.Uninstall(*dir); err != nil {
			fatal(err)
		}
		fmt.Println("plugin removed")
	case "path":
		path, err := plugin.Dir()
		if err != nil {
			fatal(err)
		}
		fmt.Println(path)
	default:
		fatal(fmt.Errorf("plugin: unknown action %q", sub))
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	o := &overrides{}
	addOverrides(fs, o)
	fs.Parse(args)

	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		fatal(fmt.Errorf("run: provide a command, e.g. kestrel run -- opencode"))
	}

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}

	base := strings.ToLower(filepath.Base(command[0]))
	if base == "opencode" || base == "opencode.exe" {
		real, err := opencode.Find()
		if err != nil {
			fatal(err)
		}
		command[0] = real
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withProxyEnv(os.Environ(), "http://"+cfg.Listen)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fatal(err)
	}
}

func withProxyEnv(base []string, proxyURL string) []string {
	noProxy := mergeNoProxy(lookupEnv(base, "NO_PROXY"))
	env := setEnv(base, "HTTPS_PROXY", proxyURL)
	env = setEnv(env, "HTTP_PROXY", proxyURL)
	env = setEnv(env, "NO_PROXY", noProxy)
	if runtime.GOOS != "windows" {
		env = setEnv(env, "https_proxy", proxyURL)
		env = setEnv(env, "http_proxy", proxyURL)
		env = setEnv(env, "no_proxy", noProxy)
	}
	return env
}

func mergeNoProxy(existing string) string {
	defaults := []string{"localhost", "127.0.0.1", "::1"}
	seen := make(map[string]bool)
	var out []string
	appendValue := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[strings.ToLower(value)] {
			return
		}
		seen[strings.ToLower(value)] = true
		out = append(out, value)
	}
	for _, value := range defaults {
		appendValue(value)
	}
	for _, value := range strings.Split(existing, ",") {
		appendValue(value)
	}
	return strings.Join(out, ",")
}

func lookupEnv(env []string, key string) string {
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(name, key) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, key+"="+value)
}
