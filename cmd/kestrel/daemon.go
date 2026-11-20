package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kestrel/internal/activity"
	"kestrel/internal/config"
	"kestrel/internal/control"
	"kestrel/internal/rotate"
	"kestrel/internal/logx"
	"kestrel/internal/observer"
	"kestrel/internal/opencode"
	"kestrel/internal/proxy"
	"kestrel/internal/resume"
	"kestrel/internal/upstream"
	"kestrel/internal/masque"
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

	logger, err := logx.New(config.LogPath(), 10<<20, 3, true)
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
		go watchParent(ctx, *parentPID, cancel)
	}

	configState := "loaded"
	if _, err := os.Stat(cfg.Path); os.IsNotExist(err) {
		configState = "defaults"
	}
	logger.Printf("kestrel %s starting config=%s (%s) listen=%s control=%s egress=%s upstream=%s",
		version, cfg.Path, configState, cfg.Listen, cfg.Control, cfg.Egress.Mode, cfg.UpstreamName())

	var (
		dialer     upstream.Dialer
		masquePool *masque.Pool
	)
	if cfg.Egress.Mode == config.ModeMasque || cfg.Egress.Mode == config.ModeUsque {
		pool, err := masque.NewPool(masque.Options{
			StateDir:    config.MasqueDir(),
			Size:        cfg.Egress.PoolSize,
			DialTimeout: time.Duration(cfg.Limits.DialTimeoutSeconds) * time.Second,
			Logger:      logger,
		})
		if err != nil {
			logger.Printf("fatal: %v", err)
			fatal(err)
		}
		bootCtx, bootCancel := context.WithTimeout(ctx, 45*time.Second)
		err = pool.Start(bootCtx)
		bootCancel()
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
	p, err := proxy.New(proxy.Options{
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
			logger.Printf("rotate enabled via masque pool (size=%d) limits=%d/h %d/day",
				cfg.Egress.PoolSize, cfg.Rotate.MaxPerHour, cfg.Rotate.MaxPerDay)
		}
	} else if cfg.Rotate.Enabled {
		logger.Printf("rotate disabled: egress mode is %s", cfg.Egress.Mode)
	} else {
		logger.Printf("rotate disabled by config")
	}

	tracker := &activity.Tracker{}

	var resumer *resume.Manager
	if cfg.Resume.Enabled {
		binary := cfg.Resume.Binary
		if binary == "" {
			found, err := opencode.Find()
			if err != nil {
				logger.Printf("warning: resume disabled: %v", err)
			} else {
				binary = found
			}
		}
		if binary != "" && opencode.IsShim(binary) {
			logger.Printf("warning: resume disabled: %s is a shim or launcher", binary)
			binary = ""
		}
		if binary != "" {
			manager, err := resume.New(resume.Options{
				Binary:  binary,
				Env:     withProxyEnv(os.Environ(), "http://"+cfg.Listen),
				Timeout: time.Duration(cfg.Resume.TimeoutSeconds) * time.Second,
				Logger:  logger,
			})
			if err != nil {
				logger.Printf("warning: resume unavailable: %v", err)
			} else {
				resumer = manager
				logger.Printf("resume enabled via %s", binary)
			}
		}
	}

	var obs *observer.Observer
	if cfg.Observer.Enabled {
		logPath := cfg.Observer.LogPath
		if logPath == "" {
			logPath = opencode.LogFile()
		}
		switch {
		case logPath == "":
			logger.Printf("warning: observer disabled: cannot resolve the OpenCode log path")
		case rotManager == nil:
			logger.Printf("warning: observer disabled: rotate is not configured")
		default:
			instance, err := observer.New(observer.Options{
				Path:      logPath,
				StatePath: config.ObserverStatePath(),
				Poll:      time.Duration(cfg.Observer.PollSeconds) * time.Second,
				Dedup:     time.Duration(cfg.Observer.DedupSeconds) * time.Second,
				Logger:    logger,
				Suppress: func() bool {
					return tracker.PluginEventWithin(time.Duration(cfg.Observer.PluginGraceSeconds) * time.Second)
				},
				OnQuota: func(session string) {
					go handleQuota(rotManager, resumer, logger, session)
				},
			})
			if err != nil {
				logger.Printf("warning: observer unavailable: %v", err)
			} else {
				obs = instance
				logger.Printf("observer watching %s", logPath)
			}
		}
	} else {
		logger.Printf("observer disabled by config")
	}

	controlOpts := control.Options{
		Listen:      cfg.Control,
		Token:       token,
		Version:     version,
		StartedAt:   started,
		ProxyListen: cfg.Listen,
		Egress:      control.Egress{Mode: cfg.Egress.Mode, Upstream: cfg.UpstreamName()},
		Stats:       p.Stats,
		Activity:    tracker,
		Logger:      logger,
	}
	if rotManager != nil {
		controlOpts.Hop = rotManager
	}
	if masquePool != nil {
		controlOpts.Pool = masquePool.Stats
	}
	if obs != nil {
		controlOpts.Observer = obs.Stats
	}
	if resumer != nil {
		controlOpts.Resume = resumer.Stats
	}
	c, err := control.New(controlOpts)
	if err != nil {
		logger.Printf("fatal: %v", err)
		fatal(err)
	}

	if obs != nil {
		go obs.Run(ctx)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- p.ListenAndServe() }()
	go func() { errCh <- c.ListenAndServe() }()

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
	logger.Printf("kestrel stopped")
}

func handleQuota(rotator *rotate.Manager, resumer *resume.Manager, logger *logx.Logger, session string) {
	if rotator == nil {
		return
	}
	start := time.Now()
	result := rotator.Hop(context.Background(), rotate.Request{Session: session, Reason: "observer.free-limit"})
	if result.Status != "ok" {
		logger.Printf("observer rotate failed session=%s dur=%s: %s",
			session, time.Since(start).Round(time.Millisecond), result.Error)
		return
	}
	logger.Printf("observer rotate ok session=%s dur=%s", session, time.Since(start).Round(time.Millisecond))
	if resumer == nil || session == "" {
		return
	}
	if err := resumer.Resume(context.Background(), session); err != nil {
		logger.Printf("observer resume failed session=%s: %v", session, err)
		return
	}
	logger.Printf("observer resume ok session=%s", session)
}
