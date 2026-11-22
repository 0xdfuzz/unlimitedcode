package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/control"
	"unlimitedcode/internal/doctor"
	"unlimitedcode/internal/logx"
	"unlimitedcode/internal/masque"
	"unlimitedcode/internal/proxy"
	"unlimitedcode/internal/rotate"
	"unlimitedcode/internal/update"
	"unlimitedcode/internal/upstream"
)

func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	o := &overrides{}
	addOverrides(fs, o)
	parentPID := fs.Int("parent-pid", 0, "exit cleanly when this parent PID terminates")
	fs.Parse(args)

	cfg, err := o.load(fs)
	if err != nil {
		fatal(err)
	}

	// Claim the control port before booting MASQUE slots so concurrent plugin
	// auto-starts (server + TUI + Desktop) exit cleanly without duplicate
	// Cloudflare registrations or bind-error spam.
	earlyLn, err := net.Listen("tcp", cfg.Control)
	if err != nil {
		return
	}
	defer func() {
		if earlyLn != nil {
			_ = earlyLn.Close()
		}
	}()

	// Clean up any leftover .old binary from a live Windows self-update swap.
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}

	logger, err := logx.New(config.LogPath(), 2<<20, 2, *parentPID == 0)
	if err != nil {
		fatal(fmt.Errorf("open log: %w", err))
	}
	defer logger.Close()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	if *parentPID > 0 {
		logger.Printf("watching parent pid=%d", *parentPID)
		go watchParent(ctx, *parentPID, cancel, logger.Printf)
	}

	configState := "loaded"
	if _, err := os.Stat(cfg.Path); os.IsNotExist(err) {
		configState = "defaults"
	}
	logger.Printf("UnlimitedCode %s starting config=%s (%s) listen=%s control=%s egress=%s upstream=%s",
		version, cfg.Path, configState, cfg.Listen, cfg.Control, cfg.Egress.Mode, cfg.UpstreamName())

	expectedProxy := "http://" + cfg.Listen
	var (
		conflictsMu     sync.RWMutex
		activeConflicts []string
		lastWARPCheck   atomic.Int64
	)
	addConflict := func(msg string) {
		conflictsMu.Lock()
		for _, existing := range activeConflicts {
			if existing == msg {
				conflictsMu.Unlock()
				return
			}
		}
		activeConflicts = append(activeConflicts, msg)
		conflictsMu.Unlock()
		logger.Printf("conflicts detected: %s", msg)
	}
	clearConflict := func(msg string) {
		conflictsMu.Lock()
		defer conflictsMu.Unlock()
		for i, existing := range activeConflicts {
			if existing == msg {
				activeConflicts = append(activeConflicts[:i], activeConflicts[i+1:]...)
				return
			}
		}
	}
	checkWARPOnFailure := func() {
		now := time.Now().Unix()
		prev := lastWARPCheck.Load()
		if now-prev < 15 || !lastWARPCheck.CompareAndSwap(prev, now) {
			return
		}
		go func() {
			cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
			defer ccancel()
			if doctor.DetectWARPConflict(cctx) {
				addConflict(doctor.ConflictWARPConnected)
			}
		}()
	}

	var (
		dialer         upstream.Dialer
		masquePool     *masque.Pool
		bootResumeOnce sync.Once
		onSlotReady    func()
	)
	var p *proxy.Server
	onSlotReady = func() {
		clearConflict(doctor.ConflictWARPConnected)
		if p != nil {
			bootResumeOnce.Do(p.Resume)
		}
	}
	if cfg.Egress.Mode == config.ModeMasque || cfg.Egress.Mode == config.ModeUsque {
		pool, err := masque.NewPool(masque.Options{
			StateDir:      config.MasqueDir(),
			Size:          cfg.Egress.PoolSize,
			DialTimeout:   time.Duration(cfg.Limits.DialTimeoutSeconds) * time.Second,
			Logger:        logger,
			OnSlotReady:   func() { onSlotReady() },
			OnDialFailure: checkWARPOnFailure,
		})
		if err != nil {
			logger.Printf("fatal: %v", err)
			fatal(err)
		}
		defer pool.Close()
		masquePool = pool
		dialer = pool
		if cfg.Egress.DirectFallback {
			dialer = upstream.Fallback{
				Primary:   pool,
				Secondary: upstream.Direct{Timeout: time.Duration(cfg.Limits.DialTimeoutSeconds) * time.Second},
				OnFallback: func(err error) {
					logger.Printf("warning: primary egress failed, falling back to direct: %v", err)
				},
			}
		}
	} else {
		var err error
		dialer, err = upstream.Build(cfg)
		if err != nil {
			logger.Printf("fatal: %v", err)
			fatal(err)
		}
		if fallback, ok := dialer.(upstream.Fallback); ok {
			fallback.OnFallback = func(err error) {
				logger.Printf("warning: primary egress failed, falling back to direct: %v", err)
			}
			dialer = fallback
		}
	}

	token, err := control.EnsureToken(config.TokenPath())
	if err != nil {
		logger.Printf("fatal: %v", err)
		fatal(fmt.Errorf("control token: %w", err))
	}

	started := time.Now()
	p, err = proxy.New(proxy.Options{
		Listen:       cfg.Listen,
		Allow:        cfg.Allow,
		AllowPorts:   cfg.AllowPorts,
		Dialer:       dialer,
		Passthrough:  cfg.Egress.PassthroughEnabled(),
		MaxTunnels:   cfg.Limits.MaxTunnels,
		IdleTimeout:  time.Duration(cfg.Limits.IdleTimeoutSeconds) * time.Second,
		DialTimeout:  time.Duration(cfg.Limits.DialTimeoutSeconds) * time.Second,
		PauseTimeout: time.Duration(cfg.Rotate.TimeoutSeconds+15) * time.Second,
		Logger:       logger,
	})
	if err != nil {
		logger.Printf("fatal: %v", err)
		fatal(err)
	}

	var rotManager *rotate.Manager
	if cfg.Rotate.Enabled && masquePool != nil {
		if manager, err := rotate.New(rotate.Options{
			Gate:        p,
			Reconnector: masquePool,
			Timeout:     time.Duration(cfg.Rotate.TimeoutSeconds) * time.Second,
			Cooldown:    time.Duration(cfg.Rotate.FailureCooldownSecs) * time.Second,
			MaxPerHour:  cfg.Rotate.MaxPerHour,
			MaxPerDay:   cfg.Rotate.MaxPerDay,
			Logger:      logger,
		}); err != nil {
			logger.Printf("warning: rotate unavailable: %v", err)
		} else {
			rotManager = manager
			if cfg.Rotate.MaxPerHour == 0 && cfg.Rotate.MaxPerDay == 0 {
				logger.Printf("rotate enabled via masque pool (size=%d) limits=unlimited", cfg.Egress.PoolSize)
			} else {
				logger.Printf("rotate enabled via masque pool (size=%d) limits=%d/h %d/day",
					cfg.Egress.PoolSize, cfg.Rotate.MaxPerHour, cfg.Rotate.MaxPerDay)
			}
		}
	} else if cfg.Rotate.Enabled {
		logger.Printf("rotate disabled: egress mode is %s", cfg.Egress.Mode)
	} else {
		logger.Printf("rotate disabled by config")
	}

	var availableUpdate atomic.Value
	availableUpdate.Store("")
	go func() {
		checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
		defer checkCancel()
		if rel, err := update.Check(checkCtx, version); err == nil && rel != nil {
			availableUpdate.Store(rel.Tag)
			logger.Printf("update available: %s (current %s)", rel.Tag, version)
		}
	}()

	go func() {
		for _, c := range doctor.DetectProxyConflicts(expectedProxy) {
			addConflict(c)
		}
	}()

	controlOpts := control.Options{
		Listen:          cfg.Control,
		Token:           token,
		Version:         version,
		UpdateAvailable: func() string { return availableUpdate.Load().(string) },
		Conflicts: func() []string {
			conflictsMu.RLock()
			defer conflictsMu.RUnlock()
			if len(activeConflicts) == 0 {
				return nil
			}
			cp := make([]string, len(activeConflicts))
			copy(cp, activeConflicts)
			return cp
		},
		OnFixConflicts: func(rctx context.Context) ([]string, []string) {
			actions, remaining := doctor.ResolveActiveConflicts(rctx, expectedProxy)
			conflictsMu.Lock()
			activeConflicts = remaining
			conflictsMu.Unlock()
			if len(actions) > 0 {
				logger.Printf("conflicts resolved: %s", strings.Join(actions, "; "))
			}
			if masquePool != nil && (len(actions) > 0 || len(remaining) == 0) {
				masquePool.KickRecovery()
			}
			return actions, remaining
		},
		StartedAt:   started,
		ProxyListen: cfg.Listen,
		Egress:      control.Egress{Mode: cfg.Egress.Mode, Upstream: cfg.UpstreamName()},
		Stats:       p.Stats,
		OnShutdown:  cancel,
		Logger:      logger,
	}
	if rotManager != nil {
		controlOpts.Hop = rotManager
	}
	if masquePool != nil {
		controlOpts.Pool = masquePool.Stats
	}
	c, err := control.New(controlOpts)
	if err != nil {
		logger.Printf("fatal: %v", err)
		fatal(err)
	}

	// Open the proxy and control ports immediately so OpenCode never hits
	// ECONNREFUSED during MASQUE boot: routed hosts wait smoothly at the
	// proxy gate until the first slot is ready, while passthrough hosts go
	// straight through with zero delay.
	errCh := make(chan error, 3)
	if masquePool != nil {
		p.Pause()
		go func() {
			bootCtx, bootCancel := context.WithTimeout(ctx, 45*time.Second)
			err := masquePool.Start(bootCtx)
			bootCancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// Keep the control server alive instead of crashing the daemon so
				// Desktop/TUI can surface the conflict prompt (e.g. WARP blocking
				// UDP 443) and KickRecovery() can bring MASQUE online in-place.
				logger.Printf("warning: masque initial boot failed (%v); staying alive for background recovery", err)
				checkWARPOnFailure()
			}
			bootResumeOnce.Do(p.Resume)
		}()
	}

	controlLn := earlyLn
	earlyLn = nil // ownership transfers to control server (no close/rebind race)
	go func() { errCh <- p.ListenAndServe() }()
	go func() { errCh <- c.Serve(controlLn) }()

	select {
	case <-ctx.Done():
		logger.Printf("signal received, shutting down")
	case err := <-errCh:
		if err != nil {
			logger.Printf("fatal: %v", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Close(shutdownCtx)
	c.Close(shutdownCtx)
	logger.Printf("UnlimitedCode stopped")
}
