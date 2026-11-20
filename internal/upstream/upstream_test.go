package upstream

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHTTPConnectRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n")
	}()

	dialer, err := NewHTTPConnect("http://"+ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", "example.com:443")
	if err == nil {
		conn.Close()
		t.Fatal("expected CONNECT to fail")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("error = %v", err)
	}
}

type stubDialer struct {
	name string
	addr string
	err  error
}

func (s stubDialer) Name() string { return s.name }

func (s stubDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if s.err != nil {
		return nil, s.err
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, s.addr)
}

func TestFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	fellBack := false
	dialer := Fallback{
		Primary:   stubDialer{name: "broken", err: io.ErrUnexpectedEOF},
		Secondary: stubDialer{name: "direct", addr: ln.Addr().String()},
		OnFallback: func(error) {
			fellBack = true
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", "ignored:1")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !fellBack {
		t.Fatal("fallback callback did not run")
	}
}
