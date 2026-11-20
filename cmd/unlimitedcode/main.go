package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"unlimitedcode/internal/config"
)

var version = "0.2.0-dev"

func main() {
	if len(os.Args) < 2 {
		cmdInstall(nil, true)
		return
	}
	switch os.Args[1] {
	case "install":
		cmdInstall(os.Args[2:], false)
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "daemon":
		cmdDaemon(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "env":
		cmdEnv(os.Args[2:])
	case "rotate", "hop":
		cmdRotate(os.Args[2:])
	case "plugin":
		cmdPlugin(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "UnlimitedCode: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `UnlimitedCode - local egress daemon for official OpenCode

usage:
  unlimitedcode install   [flags]       install plugin & proxy for OpenCode (default when run with no args)
  unlimitedcode uninstall [--purge]     remove plugin, proxy env, and optionally state
  unlimitedcode daemon    [flags]       run the proxy, MASQUE pool and control server
  unlimitedcode status    [flags]       show daemon status
  unlimitedcode doctor    [flags]       check config, ports, egress, plugin and opencode
  unlimitedcode rotate    [flags]       request one egress rotation (alias: hop)
  unlimitedcode setup     [flags]       advanced setup (--takeover, --service, --dry-run)
  unlimitedcode env       [flags]       print proxy environment variables
  unlimitedcode plugin    install|uninstall|path
  unlimitedcode run       [flags] -- <command> [args...]
  unlimitedcode version

common flags:
  --config PATH                   config file (default: state home)
  --listen ADDR                   proxy listen address
  --control ADDR                  control listen address
  --egress MODE                   masque | http-connect | direct
  --upstream URL                  http-connect upstream
  --allow HOSTS                   comma-separated hosts routed via egress
  --allow-ports PORTS             comma-separated ports (default 443,80)
  --max-tunnels N                 concurrent tunnel limit
  --direct-fallback               fall back to direct when the upstream fails
`)
}

type overrides struct {
	configPath     string
	listen         string
	controlAddr    string
	egress         string
	upstreamAddr   string
	allow          string
	allowPorts     string
	maxTunnels     int
	idleSeconds    int
	dialSeconds    int
	directFallback bool
	active         map[string]bool
}

func addConfigFlag(fs *flag.FlagSet, o *overrides) {
	o.active = make(map[string]bool)
	fs.StringVar(&o.configPath, "config", "", "config file")
}

func addOverrides(fs *flag.FlagSet, o *overrides) {
	o.active = make(map[string]bool)
	fs.StringVar(&o.configPath, "config", "", "config file")
	fs.StringVar(&o.listen, "listen", "", "proxy listen address")
	fs.StringVar(&o.controlAddr, "control", "", "control listen address")
	fs.StringVar(&o.egress, "egress", "", "masque, http-connect or direct")
	fs.StringVar(&o.upstreamAddr, "upstream", "", "http-connect upstream URL")
	fs.StringVar(&o.allow, "allow", "", "comma-separated routed hosts")
	fs.StringVar(&o.allowPorts, "allow-ports", "", "comma-separated ports")
	fs.IntVar(&o.maxTunnels, "max-tunnels", 0, "concurrent tunnel limit")
	fs.IntVar(&o.idleSeconds, "idle-timeout", 0, "idle timeout in seconds")
	fs.IntVar(&o.dialSeconds, "dial-timeout", 0, "dial timeout in seconds")
	fs.BoolVar(&o.directFallback, "direct-fallback", false, "fall back to direct egress")
}

func (o *overrides) load(fs *flag.FlagSet) (config.Config, error) {
	path := o.configPath
	if path == "" {
		path = os.Getenv("UNLIMITEDCODE_CONFIG")
	}
	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	fs.Visit(func(f *flag.Flag) { o.active[f.Name] = true })

	if o.active["listen"] {
		cfg.Listen = o.listen
	}
	if o.active["control"] {
		cfg.Control = o.controlAddr
	}
	if o.active["allow"] {
		cfg.Allow = splitCSV(o.allow)
	}
	if o.active["allow-ports"] {
		ports, err := parseInts(o.allowPorts)
		if err != nil {
			return cfg, fmt.Errorf("--allow-ports: %w", err)
		}
		cfg.AllowPorts = ports
	}
	if o.active["egress"] {
		cfg.Egress.Mode = o.egress
	}
	if o.active["upstream"] {
		cfg.Egress.Upstream = o.upstreamAddr
		if !o.active["egress"] {
			cfg.Egress.Mode = config.ModeHTTPConnect
		}
	}
	if o.active["direct-fallback"] {
		cfg.Egress.DirectFallback = o.directFallback
	}
	if o.active["max-tunnels"] {
		cfg.Limits.MaxTunnels = o.maxTunnels
	}
	if o.active["idle-timeout"] {
		cfg.Limits.IdleTimeoutSeconds = o.idleSeconds
	}
	if o.active["dial-timeout"] {
		cfg.Limits.DialTimeoutSeconds = o.dialSeconds
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseInts(value string) ([]int, error) {
	parts := splitCSV(value)
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "UnlimitedCode: %v\n", err)
	os.Exit(1)
}
