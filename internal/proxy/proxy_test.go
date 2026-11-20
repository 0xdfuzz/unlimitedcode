package proxy_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"unlimitedcode/internal/proxy"
	"unlimitedcode/internal/upstream"
)

func echoServer(t *testing.T) (addr string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	host, portText, _ := net.SplitHostPort(ln.Addr().String())
	port, _ = strconv.Atoi(portText)
	return net.JoinHostPort(host, portText), port
}

func startProxy(t *testing.T, dialer upstream.Dialer, allow []string, ports []int) (*proxy.Server, string) {
	t.Helper()
	srv, err := proxy.New(proxy.Options{
		Allow:       allow,
		AllowPorts:  ports,
		Dialer:      dialer,
		MaxTunnels:  8,
		IdleTimeout: 5 * time.Second,
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Close(ctx)
	})
	return srv, ln.Addr().String()
}

func connect(t *testing.T, proxyAddr, target string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("bad status line %q", line)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("bad status line %q", line)
	}
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if header == "\r\n" || header == "\n" {
			break
		}
	}
	return conn, br, status
}

func TestDirectTunnel(t *testing.T) {
	target, port := echoServer(t)
	srv, addr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, []string{"127.0.0.1"}, []int{port})

	conn, br, status := connect(t, addr, target)
	defer conn.Close()
	if status != 200 {
		t.Fatalf("CONNECT status = %d", status)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
	conn.Close()
	time.Sleep(100 * time.Millisecond)
	stats := srv.Stats()
	if stats.Total != 1 || stats.Failed != 0 || stats.Denied != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestDeniedHost(t *testing.T) {
	target, port := echoServer(t)
	_, addr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, []string{"example.com"}, []int{port})

	conn, _, status := connect(t, addr, target)
	defer conn.Close()
	if status != 403 {
		t.Fatalf("CONNECT status = %d, want 403", status)
	}
}

func TestDeniedPort(t *testing.T) {
	target, _ := echoServer(t)
	_, addr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, []string{"127.0.0.1"}, []int{443})

	conn, _, status := connect(t, addr, target)
	defer conn.Close()
	if status != 403 {
		t.Fatalf("CONNECT status = %d, want 403", status)
	}
}

func TestChainThroughHTTPConnect(t *testing.T) {
	target, port := echoServer(t)
	allow := []string{"127.0.0.1"}

	_, innerAddr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, allow, []int{port})
	outerDialer, err := upstream.NewHTTPConnect("http://"+innerAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, outerAddr := startProxy(t, outerDialer, allow, []int{port})

	conn, br, status := connect(t, outerAddr, target)
	defer conn.Close()
	if status != 200 {
		t.Fatalf("CONNECT status = %d", status)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("chained")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "chained" {
		t.Fatalf("echo = %q", buf)
	}
}

func TestPauseBlocksNewTunnels(t *testing.T) {
	target, port := echoServer(t)
	srv, addr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, []string{"127.0.0.1"}, []int{port})

	srv.Pause()
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer conn.Close()
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			done <- result{err: err}
			return
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			done <- result{err: fmt.Errorf("bad status %q", line)}
			return
		}
		status, _ := strconv.Atoi(fields[1])
		done <- result{status: status}
	}()

	select {
	case got := <-done:
		t.Fatalf("CONNECT completed while paused: %+v", got)
	case <-time.After(150 * time.Millisecond):
	}

	srv.Resume()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != 200 {
			t.Fatalf("CONNECT status = %d", got.status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CONNECT did not resume")
	}
}

func TestCloseTunnelsEndsActiveStream(t *testing.T) {
	target, port := echoServer(t)
	srv, addr := startProxy(t, upstream.Direct{Timeout: 2 * time.Second}, []string{"127.0.0.1"}, []int{port})

	conn, br, status := connect(t, addr, target)
	defer conn.Close()
	if status != 200 {
		t.Fatalf("CONNECT status = %d", status)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(br, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}

	srv.CloseTunnels()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("expected closed tunnel")
	}
}
