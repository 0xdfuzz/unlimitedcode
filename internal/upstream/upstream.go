package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"unlimitedcode/internal/config"
)

// Dialer opens a tunnel to addr (host:port) through some egress.
type Dialer interface {
	Name() string
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type Direct struct {
	Timeout time.Duration
}

func (d Direct) Name() string { return "direct" }

func (d Direct) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	nd := net.Dialer{Timeout: d.Timeout}
	return nd.DialContext(ctx, network, addr)
}

type HTTPConnect struct {
	URL     *url.URL
	Timeout time.Duration
}

func (h *HTTPConnect) Name() string { return "http-connect " + h.URL.Host }

func (h *HTTPConnect) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("http-connect: unsupported network %q", network)
	}

	proxyAddr := h.URL.Host
	if h.URL.Port() == "" {
		port := "80"
		if h.URL.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(h.URL.Hostname(), port)
	}

	nd := net.Dialer{Timeout: h.Timeout}
	conn, err := nd.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream %s: %w", proxyAddr, err)
	}
	cleanup := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}

	if h.Timeout > 0 {
		conn.SetDeadline(time.Now().Add(h.Timeout))
	}

	if h.URL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: h.URL.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return cleanup(fmt.Errorf("tls to upstream %s: %w", proxyAddr, err))
		}
		conn = tlsConn
	}

	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if h.URL.User != nil {
		password, _ := h.URL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(h.URL.User.Username() + ":" + password))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", token)
	}
	req.WriteString("Connection: keep-alive\r\n\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return cleanup(fmt.Errorf("write CONNECT to upstream %s: %w", proxyAddr, err))
	}

	br := bufio.NewReader(conn)
	status, reason, err := readConnectResponse(br)
	if err != nil {
		return cleanup(fmt.Errorf("read CONNECT from upstream %s: %w", proxyAddr, err))
	}
	if status != http.StatusOK {
		return cleanup(fmt.Errorf("upstream %s refused CONNECT %s: %d %s", proxyAddr, addr, status, reason))
	}

	conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: br}, nil
}

func readConnectResponse(br *bufio.Reader) (int, string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, "", fmt.Errorf("malformed status line %q", line)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", fmt.Errorf("malformed status line %q", line)
	}
	reason := ""
	if len(parts) == 3 {
		reason = parts[2]
	}
	for i := 0; i < 100; i++ {
		header, err := br.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		if header == "\r\n" || header == "\n" {
			return status, reason, nil
		}
	}
	return 0, "", errors.New("upstream sent too many headers")
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

type Fallback struct {
	Primary    Dialer
	Secondary  Dialer
	OnFallback func(error)
}

func (f Fallback) Name() string {
	return f.Primary.Name() + " + fallback " + f.Secondary.Name()
}

func (f Fallback) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := f.Primary.DialContext(ctx, network, addr)
	if err == nil {
		return conn, nil
	}
	if f.OnFallback != nil {
		f.OnFallback(err)
	}
	return f.Secondary.DialContext(ctx, network, addr)
}

func Build(cfg config.Config) (Dialer, error) {
	timeout := time.Duration(cfg.Limits.DialTimeoutSeconds) * time.Second
	var primary Dialer

	switch cfg.Egress.Mode {
	case config.ModeDirect:
		return Direct{Timeout: timeout}, nil
	case config.ModeHTTPConnect:
		d, err := parseHTTPConnect(cfg.Egress.Upstream, timeout)
		if err != nil {
			return nil, fmt.Errorf("egress.upstream: %w", err)
		}
		primary = d
	default:
		return nil, fmt.Errorf("unknown egress mode %q", cfg.Egress.Mode)
	}

	if cfg.Egress.DirectFallback {
		return Fallback{Primary: primary, Secondary: Direct{Timeout: timeout}}, nil
	}
	return primary, nil
}

func NewHTTPConnect(raw string, timeout time.Duration) (*HTTPConnect, error) {
	return parseHTTPConnect(raw, timeout)
}

func parseHTTPConnect(raw string, timeout time.Duration) (*HTTPConnect, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("path is not supported, got %q", u.Path)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPConnect{URL: u, Timeout: timeout}, nil
}
