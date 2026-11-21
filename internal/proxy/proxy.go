package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/upstream"
)

type Logger interface {
	Printf(format string, args ...any)
}

type Options struct {
	Listen       string
	Allow        []string
	AllowPorts   []int
	Dialer       upstream.Dialer
	Passthrough  bool // forward non-allowlisted public hosts directly instead of returning 403
	MaxTunnels   int
	IdleTimeout  time.Duration
	DialTimeout  time.Duration
	PauseTimeout time.Duration
	Logger       Logger
}

type Stats struct {
	Active    int64  `json:"active"`
	Total     int64  `json:"total"`
	Denied    int64  `json:"denied"`
	Failed    int64  `json:"failed"`
	BytesUp   int64  `json:"bytes_up"`
	BytesDown int64  `json:"bytes_down"`
	LastEvent string `json:"last_event,omitempty"`
}

type Server struct {
	opts    Options
	logger  Logger
	srv     *http.Server
	ln      net.Listener
	sem     chan struct{}
	started time.Time

	active    atomic.Int64
	total     atomic.Int64
	denied    atomic.Int64
	failed    atomic.Int64
	bytesUp   atomic.Int64
	bytesDown atomic.Int64

	mu        sync.Mutex
	tunnels   map[net.Conn]struct{}
	lastEvent string

	gateMu   sync.Mutex
	gateOpen bool
	gateCh   chan struct{}
}

func New(opts Options) (*Server, error) {
	if opts.Dialer == nil {
		return nil, errors.New("proxy: dialer is required")
	}
	if len(opts.AllowPorts) == 0 {
		return nil, errors.New("proxy: allow ports are required")
	}
	if opts.MaxTunnels < 1 {
		opts.MaxTunnels = 128
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.PauseTimeout <= 0 {
		opts.PauseTimeout = 90 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{
		opts:     opts,
		logger:   logger,
		sem:      make(chan struct{}, opts.MaxTunnels),
		started:  time.Now(),
		tunnels:  make(map[net.Conn]struct{}),
		gateOpen: true,
		gateCh:   make(chan struct{}),
	}, nil
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func (s *Server) Serve(ln net.Listener) error {
	s.ln = ln
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 15 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(&logWriter{s}, "", 0),
	}
	s.logger.Printf("proxy listening on %s", ln.Addr())
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Addr() string {
	if s.ln == nil {
		return s.opts.Listen
	}
	return s.ln.Addr().String()
}

func (s *Server) Close(ctx context.Context) error {
	var err error
	if s.srv != nil {
		err = s.srv.Shutdown(ctx)
	}
	s.CloseTunnels()
	return err
}

// Pause stops new tunnels from dialing until Resume. Existing tunnels keep
// running until CloseTunnels is called.
func (s *Server) Pause() {
	s.gateMu.Lock()
	if s.gateOpen {
		s.gateOpen = false
		s.gateCh = make(chan struct{})
	}
	s.gateMu.Unlock()
}

func (s *Server) Resume() {
	s.gateMu.Lock()
	if !s.gateOpen {
		s.gateOpen = true
		close(s.gateCh)
	}
	s.gateMu.Unlock()
}

func (s *Server) waitGate(ctx context.Context) error {
	s.gateMu.Lock()
	if s.gateOpen {
		s.gateMu.Unlock()
		return nil
	}
	ch := s.gateCh
	s.gateMu.Unlock()

	timer := time.NewTimer(s.opts.PauseTimeout)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return errors.New("egress switch took too long")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CloseTunnels closes every tracked client and upstream connection. The
// handlers finish on their own and clear the map entries.
func (s *Server) CloseTunnels() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.tunnels))
	for conn := range s.tunnels {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	for _, conn := range conns {
		conn.Close()
	}
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	last := s.lastEvent
	s.mu.Unlock()
	return Stats{
		Active:    s.active.Load(),
		Total:     s.total.Load(),
		Denied:    s.denied.Load(),
		Failed:    s.failed.Load(),
		BytesUp:   s.bytesUp.Load(),
		BytesDown: s.bytesDown.Load(),
		LastEvent: last,
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		s.denied.Add(1)
		s.event("reject method=%s path=%s", r.Method, r.URL.Path)
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}

	authority := r.Host
	if authority == "" {
		authority = r.URL.Host
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		s.denied.Add(1)
		s.event("reject malformed authority=%q", authority)
		http.Error(w, "malformed authority", http.StatusBadRequest)
		return
	}
	portNum, err := strconv.Atoi(port)
	if err != nil || !s.portAllowed(portNum) {
		s.denied.Add(1)
		s.event("deny authority=%s reason=port", authority)
		http.Error(w, "port denied", http.StatusForbidden)
		return
	}

	routedViaEgress := config.HostAllowed(host, s.opts.Allow)
	if !routedViaEgress && !s.opts.Passthrough {
		s.denied.Add(1)
		s.event("deny authority=%s reason=host", authority)
		http.Error(w, "host denied", http.StatusForbidden)
		return
	}

	if routedViaEgress {
		if err := s.waitGate(r.Context()); err != nil {
			s.failed.Add(1)
			s.event("deny authority=%s reason=egress-switch", authority)
			http.Error(w, "egress switch in progress", http.StatusServiceUnavailable)
			return
		}
	}

	select {
	case s.sem <- struct{}{}:
	default:
		s.failed.Add(1)
		s.event("deny authority=%s reason=tunnel-limit", authority)
		http.Error(w, "tunnel limit reached", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-s.sem }()

	dialer := s.opts.Dialer
	if !routedViaEgress {
		dialer = upstream.Direct{Timeout: s.opts.DialTimeout}
	}

	dialCtx, cancel := context.WithTimeout(context.Background(), s.opts.DialTimeout)
	defer cancel()
	remote, err := dialer.DialContext(dialCtx, "tcp", authority)
	if err != nil {
		s.failed.Add(1)
		s.event("dial-fail authority=%s upstream=%q err=%v", authority, dialer.Name(), err)
		s.logger.Printf("CONNECT %s failed via %s: %v", authority, dialer.Name(), err)
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		remote.Close()
		s.failed.Add(1)
		s.event("hijack-fail authority=%s", authority)
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		remote.Close()
		s.failed.Add(1)
		s.event("hijack-fail authority=%s err=%v", authority, err)
		return
	}

	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		remote.Close()
		s.failed.Add(1)
		s.event("write-fail authority=%s", authority)
		return
	}
	if err := rw.Flush(); err != nil {
		client.Close()
		remote.Close()
		s.failed.Add(1)
		s.event("write-fail authority=%s", authority)
		return
	}

	s.total.Add(1)
	s.active.Add(1)
	if routedViaEgress {
		s.track(client, true)
		s.track(remote, true)
	}
	start := time.Now()
	upstreamName := dialer.Name()
	s.event("connect authority=%s upstream=%q", authority, upstreamName)

	clientConn := net.Conn(client)
	remoteConn := net.Conn(remote)
	if s.opts.IdleTimeout > 0 {
		clientConn = idleConn{Conn: clientConn, idle: s.opts.IdleTimeout}
		remoteConn = idleConn{Conn: remoteConn, idle: s.opts.IdleTimeout}
	}

	// rw.Reader may already hold bytes that arrived with the CONNECT request,
	// so the client to upstream copy must start there.
	var up, down int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(remoteConn, rw.Reader)
		s.bytesUp.Add(n)
		up = n
		closeWrite(remote)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(clientConn, remoteConn)
		s.bytesDown.Add(n)
		down = n
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	// Bound half-closed drain so abandoned sockets release goroutines and FDs quickly.
	drain := time.Now().Add(2 * time.Second)
	_ = client.SetDeadline(drain)
	_ = remote.SetDeadline(drain)
	<-done
	client.Close()
	remote.Close()

	s.active.Add(-1)
	if routedViaEgress {
		s.track(client, false)
		s.track(remote, false)
	}
	s.event("close authority=%s dur=%s up=%d down=%d",
		authority,
		time.Since(start).Round(time.Millisecond),
		up,
		down,
	)
}

func (s *Server) portAllowed(port int) bool {
	for _, allowed := range s.opts.AllowPorts {
		if port == allowed {
			return true
		}
	}
	return false
}

func (s *Server) track(conn net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.tunnels[conn] = struct{}{}
	} else {
		delete(s.tunnels, conn)
	}
}

func (s *Server) event(format string, args ...any) {
	line := time.Now().Format("15:04:05.000") + " " + fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.lastEvent = line
	s.mu.Unlock()
}

type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}


func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

type logWriter struct{ s *Server }

func (w *logWriter) Write(p []byte) (int, error) {
	w.s.logger.Printf("http: %s", string(p))
	return len(p), nil
}
