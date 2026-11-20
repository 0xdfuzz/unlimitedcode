package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"unlimitedcode/internal/activity"
	"unlimitedcode/internal/rotate"
	"unlimitedcode/internal/observer"
	"unlimitedcode/internal/proxy"
	"unlimitedcode/internal/resume"
	"unlimitedcode/internal/masque"
)

type Logger interface {
	Printf(format string, args ...any)
}

type Egress struct {
	Mode     string `json:"mode"`
	Upstream string `json:"upstream"`
}

type Status struct {
	Version       string          `json:"version"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	Listen        string          `json:"listen"`
	Control       string          `json:"control"`
	Egress        Egress          `json:"egress"`
	Proxy         proxy.Stats     `json:"proxy"`
	HopEnabled    bool             `json:"hop_enabled"`
	Hop           rotate.Stats        `json:"hop"`
	Pool          *masque.PoolStats `json:"pool,omitempty"`
	Observer      *observer.Stats  `json:"observer,omitempty"`
	Resume        *resume.Stats    `json:"resume,omitempty"`
}

// Hopper is implemented by rotate.Manager.
type Hopper interface {
	Hop(context.Context, rotate.Request) rotate.Result
	Stats() rotate.Stats
}

type Pauser interface {
	SetPaused(bool)
	Paused() bool
}

type Options struct {
	Listen      string
	Token       string
	Version     string
	StartedAt   time.Time
	ProxyListen string
	Egress      Egress
	Stats       func() proxy.Stats
	Hop         Hopper
	Pool        func() masque.PoolStats
	Observer    func() observer.Stats
	Resume      func() resume.Stats
	Activity    *activity.Tracker
	Logger      Logger
}

type Server struct {
	opts   Options
	logger Logger
	srv    *http.Server
	ln     net.Listener
}

func New(opts Options) (*Server, error) {
	if opts.Listen == "" {
		return nil, errors.New("control: listen is required")
	}
	if opts.Token == "" {
		return nil, errors.New("control: token is required")
	}
	if opts.Stats == nil {
		opts.Stats = func() proxy.Stats { return proxy.Stats{} }
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{opts: opts, logger: logger}, nil
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	s.logger.Printf("control listening on %s", ln.Addr())
	err = s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/status", s.auth(s.status))
	mux.HandleFunc("/rotate", s.auth(s.hopHandler))
	mux.HandleFunc("/hop", s.auth(s.hopHandler))
	mux.HandleFunc("/pause", s.auth(s.pauseHandler))
	mux.HandleFunc("/event", s.auth(s.eventHandler))
	return mux
}

func (s *Server) Addr() string {
	if s.ln == nil {
		return s.opts.Listen
	}
	return s.ln.Addr().String()
}

func (s *Server) Close(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !loopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "version": s.opts.Version})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status := Status{
		Version:       s.opts.Version,
		StartedAt:     s.opts.StartedAt,
		UptimeSeconds: int64(time.Since(s.opts.StartedAt).Seconds()),
		Listen:        s.opts.ProxyListen,
		Control:       s.opts.Listen,
		Egress:        s.opts.Egress,
		Proxy:         s.opts.Stats(),
		HopEnabled:    s.opts.Hop != nil,
	}
	if s.opts.Hop != nil {
		status.Hop = s.opts.Hop.Stats()
	}
	if s.opts.Pool != nil {
		stats := s.opts.Pool()
		status.Pool = &stats
	}
	if s.opts.Observer != nil {
		stats := s.opts.Observer()
		status.Observer = &stats
	}
	if s.opts.Resume != nil {
		stats := s.opts.Resume()
		status.Resume = &stats
	}
	writeJSON(w, status)
}

type event struct {
	Type    string `json:"type"`
	Session string `json:"session"`
	Model   string `json:"model"`
	Agent   string `json:"agent"`
	Attempt int    `json:"attempt"`
}

func (s *Server) hopHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev event
	if err := decodeJSON(w, r, &ev); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid event: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.runHop(w, r, ev, "manual")
}

func (s *Server) eventHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev event
	if err := decodeJSON(w, r, &ev); err != nil {
		http.Error(w, "invalid event: "+err.Error(), http.StatusBadRequest)
		return
	}
	if ev.Type == "" {
		http.Error(w, "type is required", http.StatusBadRequest)
		return
	}
	if ev.Type != "provider.free-limit" {
		writeJSON(w, rotate.Result{Status: "ignored"})
		return
	}
	s.opts.Activity.MarkPluginEvent()
	s.runHop(w, r, ev, ev.Type)
}

type pausePayload struct {
	Paused *bool `json:"paused,omitempty"`
}

func (s *Server) pauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	pauser, ok := s.opts.Hop.(Pauser)
	if !ok || pauser == nil {
		writeJSON(w, map[string]any{"status": "disabled", "paused": false})
		return
	}
	var p pausePayload
	if err := decodeJSON(w, r, &p); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	next := !pauser.Paused()
	if p.Paused != nil {
		next = *p.Paused
	}
	pauser.SetPaused(next)
	writeJSON(w, map[string]any{"status": "ok", "paused": next})
}

func (s *Server) runHop(w http.ResponseWriter, r *http.Request, ev event, reason string) {
	if s.opts.Hop == nil {
		writeJSON(w, rotate.Result{Status: "disabled"})
		return
	}
	writeJSON(w, s.opts.Hop.Hop(r.Context(), rotate.Request{
		Session: ev.Session,
		Model:   ev.Model,
		Agent:   ev.Agent,
		Reason:  reason,
		Attempt: ev.Attempt,
	}))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	return json.NewDecoder(r.Body).Decode(value)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(s.opts.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

// EnsureToken reads the control token, creating it with 32 random bytes when
// missing. The daemon creates it; status only reads it.
func EnsureToken(path string) (string, error) {
	token, err := ReadToken(path)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token = hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func ReadToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < 32 {
		return "", errors.New("control token is too short")
	}
	return token, nil
}

func URL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimSuffix(addr, "/")
	}
	return "http://" + addr
}
