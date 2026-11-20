package control_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"unlimitedcode/internal/control"
	"unlimitedcode/internal/rotate"
	"unlimitedcode/internal/proxy"
	"unlimitedcode/internal/upstream"
)

type testEcho struct {
	addr string
	port int
}

func newEcho(t *testing.T) testEcho {
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
	port, _ := strconv.Atoi(portText)
	return testEcho{addr: net.JoinHostPort(host, portText), port: port}
}

type testReconnector struct {
	calls atomic.Int64
}

func (r *testReconnector) Name() string { return "fake-warp" }

func (r *testReconnector) Reconnect(ctx context.Context) error {
	r.calls.Add(1)
	return nil
}

func openTunnel(t *testing.T, proxyAddr, target string) (net.Conn, *bufio.Reader, int) {
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
	status, _ := strconv.Atoi(fields[1])
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

func TestEventHopsAndClosesTunnel(t *testing.T) {
	echo := newEcho(t)
	p, err := proxy.New(proxy.Options{
		Allow:        []string{"127.0.0.1"},
		AllowPorts:   []int{echo.port},
		Dialer:       upstream.Direct{Timeout: 2 * time.Second},
		MaxTunnels:   4,
		IdleTimeout:  time.Second,
		DialTimeout:  time.Second,
		PauseTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Close(ctx)
	})

	rec := &testReconnector{}
	manager, err := rotate.New(rotate.Options{
		Gate:        p,
		Reconnector: rec,
		Timeout:     2 * time.Second,
		Cooldown:    0,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := control.New(control.Options{
		Listen:    "127.0.0.1:0",
		Token:     token,
		Version:   "test",
		StartedAt: time.Now(),
		Egress:    control.Egress{Mode: "warp"},
		Stats:     p.Stats,
		Hop:       manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	first, br, status := openTunnel(t, ln.Addr().String(), echo.addr)
	defer first.Close()
	if status != 200 {
		t.Fatalf("first CONNECT = %d", status)
	}
	first.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := first.Write([]byte("before")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "before" {
		t.Fatalf("echo = %q", buf)
	}

	body := bytes.NewBufferString(`{"type":"provider.free-limit","session":"ses_e2e","model":"m","agent":"build","attempt":2}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/event", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result rotate.Result
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("hop result = %+v", result)
	}
	if rec.calls.Load() != 1 {
		t.Fatalf("reconnects = %d", rec.calls.Load())
	}

	first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("old tunnel stayed open after hop")
	}

	second, br2, status := openTunnel(t, ln.Addr().String(), echo.addr)
	defer second.Close()
	if status != 200 {
		t.Fatalf("second CONNECT = %d", status)
	}
	second.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := second.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	buf2 := make([]byte, 5)
	if _, err := io.ReadFull(br2, buf2); err != nil {
		t.Fatal(err)
	}
	if string(buf2) != "after" {
		t.Fatalf("echo = %q", buf2)
	}

	stats := manager.Stats()
	if stats.Count != 1 || stats.Failures != 0 || stats.Busy {
		t.Fatalf("stats = %+v", stats)
	}
}
