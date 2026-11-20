// Package masque manages an in-process pool of Cloudflare WARP MASQUE L4
// clients over HTTP/3 CONNECT streams.
//
// With pool size >= 2, one slot serves active traffic while spare slots stay
// warm on distinct IPv6 /64 prefixes. A rotation swaps the active slot
// immediately (0ms) and recycles the burned slot asynchronously via key
// re-enrollment, avoiding previously burned prefixes until the daily UTC reset.
package masque

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultPoolSize = 2
	maxRecycleTries = 5
	maxBurntEntries = 200
)

type Logger interface {
	Printf(format string, args ...any)
}

type Options struct {
	StateDir    string
	Size        int
	DialTimeout time.Duration
	Logger      Logger
}

type SlotStatus struct {
	ID        int    `json:"id"`
	State     string `json:"state"` // starting | ready | recycling | failed
	Active    bool   `json:"active"`
	IP        string `json:"ip,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Colo      string `json:"colo,omitempty"`
	LastError string `json:"last_error,omitempty"`
	Recycles  int64  `json:"recycles"`
}

type PoolStats struct {
	Size       int          `json:"size"`
	ActiveSlot int          `json:"active_slot"`
	ActiveIP   string       `json:"active_ip,omitempty"`
	BurntCount int          `json:"burnt_count"`
	Slots      []SlotStatus `json:"slots"`
}

type slot struct {
	id         int
	configPath string

	mu       sync.Mutex
	cfg      slotConfig
	client   *l4Client
	state    string
	ip       string
	prefix   string
	colo     string
	lastErr  string
	recycles int64
}

type aaaaEntry struct {
	ip      string
	expires time.Time
}

type Pool struct {
	stateDir string
	size     int
	timeout  time.Duration
	log      Logger

	mu        sync.Mutex
	slots     []*slot
	active    int
	burnt     map[string]time.Time
	aaaaCache map[string]aaaaEntry
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewPool(opts Options) (*Pool, error) {
	if opts.Size <= 0 {
		opts.Size = defaultPoolSize
	}
	if opts.Size > 8 {
		return nil, fmt.Errorf("masque: pool size %d exceeds max 8", opts.Size)
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.StateDir == "" {
		return nil, errors.New("masque: state dir is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("masque: create state dir: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		stateDir:  opts.StateDir,
		size:      opts.Size,
		timeout:   opts.DialTimeout,
		log:       logger,
		burnt:     make(map[string]time.Time),
		aaaaCache: make(map[string]aaaaEntry),
		ctx:       ctx,
		cancel:    cancel,
	}

	p.slots = make([]*slot, opts.Size)
	for i := 0; i < opts.Size; i++ {
		p.slots[i] = &slot{
			id:         i,
			configPath: filepath.Join(opts.StateDir, fmt.Sprintf("slot-%d.json", i)),
			state:      "starting",
		}
	}
	return p, nil
}

// Start loads or registers slot credentials and verifies each slot's IPv6 WARP
// egress in parallel.
func (p *Pool) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	readyCh := make(chan int, p.size)
	errCh := make(chan error, p.size)

	for _, s := range p.slots {
		wg.Add(1)
		go func(sl *slot) {
			defer wg.Done()
			if err := p.bootSlot(ctx, sl); err != nil {
				p.log.Printf("masque slot=%d boot failed: %v", sl.id, err)
				errCh <- fmt.Errorf("slot %d: %w", sl.id, err)
				return
			}
			readyCh <- sl.id
		}(s)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		p.Close()
		return ctx.Err()
	}

	close(readyCh)
	close(errCh)

	readyCount := 0
	for id := range readyCh {
		if readyCount == 0 {
			p.mu.Lock()
			p.active = id
			p.mu.Unlock()
		}
		readyCount++
	}
	if readyCount == 0 {
		var errs []string
		for err := range errCh {
			errs = append(errs, err.Error())
		}
		p.Close()
		return fmt.Errorf("masque pool failed to start any slot: %s", strings.Join(errs, "; "))
	}

	// If spare slots landed on the same /64 prefix as the active slot, recycle
	// them asynchronously so the first rotation is instant.
	active := p.activeSlot()
	active.mu.Lock()
	activePrefix := active.prefix
	active.mu.Unlock()
	if activePrefix != "" {
		for _, s := range p.slots {
			if s.id == active.id {
				continue
			}
			s.mu.Lock()
			dup := s.state == "ready" && s.prefix == activePrefix
			s.mu.Unlock()
			if dup {
				p.log.Printf("masque slot=%d shares prefix %s with active slot=%d; recycling in background", s.id, activePrefix, active.id)
				p.wg.Add(1)
				go func(sl *slot) {
					defer p.wg.Done()
					bgCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
					defer cancel()
					_ = p.recycleSlot(bgCtx, sl, activePrefix)
				}(s)
			}
		}
	}

	p.log.Printf("masque in-process pool ready slots=%d/%d active=%d", readyCount, p.size, active.id)
	return nil
}

func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.cancel()
	for _, s := range p.slots {
		p.closeSlotClient(s)
	}
	p.wg.Wait()
}

// Name implements upstream.Dialer.
func (p *Pool) Name() string {
	s := p.activeSlot()
	s.mu.Lock()
	ip := s.ip
	s.mu.Unlock()
	if ip != "" {
		return fmt.Sprintf("masque[%d] (%s)", s.id, ip)
	}
	return fmt.Sprintf("masque[%d]", s.id)
}

// DialContext implements upstream.Dialer.
// It resolves hostname targets to IPv6 (AAAA via OS or DoH) when available so
// traffic exits via the slot's unique IPv6 /64 prefix instead of Cloudflare's
// shared IPv4 NAT.
func (p *Pool) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("masque: unsupported network %q", network)
	}
	target := p.resolveIPv6Target(ctx, addr)
	s := p.activeSlot()
	conn, err := p.dialSlot(ctx, s, target, addr)
	if err == nil {
		return conn, nil
	}
	if alt := p.pickOtherReady(s.id); alt != nil {
		p.mu.Lock()
		p.active = alt.id
		p.mu.Unlock()
		p.log.Printf("masque active slot=%d dial failed, failing over to slot=%d", s.id, alt.id)
		return p.dialSlot(ctx, alt, target, addr)
	}
	return nil, err
}

func (p *Pool) dialSlot(ctx context.Context, s *slot, target, fallbackAddr string) (net.Conn, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("masque slot %d is not connected", s.id)
	}
	conn, err := client.DialContext(ctx, target)
	if err == nil || target == fallbackAddr {
		return conn, err
	}
	return client.DialContext(ctx, fallbackAddr)
}

// Reconnect implements hop.Reconnector.
func (p *Pool) Reconnect(ctx context.Context) error {
	old := p.activeSlot()
	old.mu.Lock()
	oldPrefix := old.prefix
	oldIP := old.ip
	old.mu.Unlock()

	if oldPrefix != "" {
		p.markBurnt(oldPrefix)
	}

	if next := p.pickWarmSpare(old.id, oldPrefix); next != nil {
		p.mu.Lock()
		p.active = next.id
		p.mu.Unlock()

		next.mu.Lock()
		nextIP := next.ip
		next.mu.Unlock()
		p.log.Printf("masque fast-rotate slot=%d (%s) -> slot=%d (%s); recycling slot=%d in background",
			old.id, oldIP, next.id, nextIP, old.id)

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			bgCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
			defer cancel()
			if err := p.recycleSlot(bgCtx, old, oldPrefix); err != nil {
				p.log.Printf("masque bg recycle slot=%d failed: %v", old.id, err)
			}
		}()
		return nil
	}

	p.log.Printf("masque sync-rotate recycling slot=%d (%s)", old.id, oldIP)
	if err := p.recycleSlot(ctx, old, oldPrefix); err != nil {
		if alt := p.pickOtherReady(old.id); alt != nil {
			p.mu.Lock()
			p.active = alt.id
			p.mu.Unlock()
			return nil
		}
		return err
	}
	return nil
}

func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	activeID := p.active
	p.pruneBurntLocked(time.Now())
	burntCount := len(p.burnt)
	p.mu.Unlock()

	out := PoolStats{
		Size:       p.size,
		ActiveSlot: activeID,
		BurntCount: burntCount,
		Slots:      make([]SlotStatus, 0, len(p.slots)),
	}
	for _, s := range p.slots {
		s.mu.Lock()
		st := SlotStatus{
			ID:        s.id,
			State:     s.state,
			Active:    s.id == activeID,
			IP:        s.ip,
			Prefix:    s.prefix,
			Colo:      s.colo,
			LastError: s.lastErr,
			Recycles:  s.recycles,
		}
		if st.Active {
			out.ActiveIP = s.ip
		}
		s.mu.Unlock()
		out.Slots = append(out.Slots, st)
	}
	return out
}

func (p *Pool) activeSlot() *slot {
	p.mu.Lock()
	idx := p.active
	p.mu.Unlock()
	if idx < 0 || idx >= len(p.slots) {
		return p.slots[0]
	}
	return p.slots[idx]
}

func (p *Pool) pickWarmSpare(excludeID int, avoidPrefix string) *slot {
	p.mu.Lock()
	p.pruneBurntLocked(time.Now())
	p.mu.Unlock()

	var fallback *slot
	for offset := 1; offset < len(p.slots); offset++ {
		s := p.slots[(excludeID+offset)%len(p.slots)]
		s.mu.Lock()
		ready := s.state == "ready"
		prefix := s.prefix
		s.mu.Unlock()
		if !ready {
			continue
		}
		if prefix != "" && prefix != avoidPrefix && !p.isBurnt(prefix) {
			return s
		}
		if fallback == nil && prefix != avoidPrefix {
			fallback = s
		}
	}
	return fallback
}

func (p *Pool) pickOtherReady(excludeID int) *slot {
	for offset := 1; offset < len(p.slots); offset++ {
		s := p.slots[(excludeID+offset)%len(p.slots)]
		s.mu.Lock()
		ready := s.state == "ready"
		s.mu.Unlock()
		if ready {
			return s
		}
	}
	return nil
}

func (p *Pool) bootSlot(ctx context.Context, s *slot) error {
	cfg, err := loadSlotConfig(s.configPath)
	if err != nil {
		p.log.Printf("masque slot=%d registering new WARP device", s.id)
		cfg, err = registerAndEnroll(ctx)
		if err != nil {
			s.setFailed(err)
			return err
		}
		if err := saveSlotConfig(s.configPath, cfg); err != nil {
			s.setFailed(err)
			return fmt.Errorf("save slot config: %w", err)
		}
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return p.connectAndVerify(ctx, s, cfg)
}

func (p *Pool) recycleSlot(ctx context.Context, s *slot, avoidPrefix string) error {
	s.mu.Lock()
	s.state = "recycling"
	prev := s.cfg
	s.mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= maxRecycleTries; attempt++ {
		if ctx.Err() != nil {
			s.setFailed(ctx.Err())
			return ctx.Err()
		}
		p.closeSlotClient(s)

		var nextCfg slotConfig
		var err error
		if prev.ID != "" && prev.AccessToken != "" {
			nextCfg, err = enrollNewKey(ctx, prev)
		} else {
			nextCfg, err = registerAndEnroll(ctx)
		}
		if err != nil {
			lastErr = err
			p.log.Printf("masque slot=%d recycle try=%d enroll error: %v", s.id, attempt, err)
			continue
		}
		if err := saveSlotConfig(s.configPath, nextCfg); err != nil {
			lastErr = err
			p.log.Printf("masque slot=%d recycle try=%d save error: %v", s.id, attempt, err)
			continue
		}
		prev = nextCfg

		if err := p.connectAndVerify(ctx, s, nextCfg); err != nil {
			lastErr = err
			p.log.Printf("masque slot=%d recycle try=%d verify error: %v", s.id, attempt, err)
			continue
		}

		s.mu.Lock()
		s.cfg = nextCfg
		gotPrefix := s.prefix
		gotIP := s.ip
		s.mu.Unlock()

		if (gotPrefix != avoidPrefix && !p.isBurnt(gotPrefix)) || attempt == maxRecycleTries {
			s.mu.Lock()
			s.recycles++
			s.mu.Unlock()
			p.log.Printf("masque slot=%d recycled try=%d ip=%s prefix=%s", s.id, attempt, gotIP, gotPrefix)
			return nil
		}
		p.log.Printf("masque slot=%d recycle try=%d landed on burnt/same prefix %s (%s), retrying", s.id, attempt, gotPrefix, gotIP)
	}
	if lastErr == nil {
		lastErr = errors.New("recycle failed")
	}
	s.setFailed(lastErr)
	return lastErr
}

func (p *Pool) connectAndVerify(ctx context.Context, s *slot, cfg slotConfig) error {
	p.closeSlotClient(s)

	client, err := newL4Client(cfg)
	if err != nil {
		s.setFailed(err)
		return err
	}

	var trace map[string]string
	var lastErr error
	for i := 0; i < 3; i++ {
		trace, lastErr = fetchTrace(ctx, client)
		if lastErr == nil && (trace["warp"] == "on" || trace["warp"] == "plus") && trace["ip"] != "" {
			break
		}
		select {
		case <-ctx.Done():
			_ = client.Close()
			s.setFailed(ctx.Err())
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	if lastErr != nil {
		_ = client.Close()
		s.setFailed(lastErr)
		return fmt.Errorf("trace failed: %w", lastErr)
	}
	if trace["warp"] != "on" && trace["warp"] != "plus" {
		_ = client.Close()
		err := fmt.Errorf("trace reported warp=%s", trace["warp"])
		s.setFailed(err)
		return err
	}

	ip := trace["ip"]
	prefix := NormalizePrefix(ip)
	s.mu.Lock()
	s.client = client
	s.state = "ready"
	s.ip = ip
	s.prefix = prefix
	s.colo = trace["colo"]
	s.lastErr = ""
	s.mu.Unlock()
	return nil
}

func (p *Pool) closeSlotClient(s *slot) {
	s.mu.Lock()
	client := s.client
	s.client = nil
	s.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (s *slot) setFailed(err error) {
	s.mu.Lock()
	s.state = "failed"
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
}

func (p *Pool) resolveIPv6Target(ctx context.Context, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return addr
	}
	key := strings.ToLower(host)
	now := time.Now()
	p.mu.Lock()
	if entry, ok := p.aaaaCache[key]; ok && now.Before(entry.expires) {
		p.mu.Unlock()
		if entry.ip != "" {
			return net.JoinHostPort(entry.ip, port)
		}
		return addr
	}
	p.mu.Unlock()

	ip := lookupAAAA(ctx, key)
	ttl := 5 * time.Minute
	if ip == "" {
		ttl = 60 * time.Second
	}
	p.mu.Lock()
	p.aaaaCache[key] = aaaaEntry{ip: ip, expires: now.Add(ttl)}
	p.mu.Unlock()
	if ip != "" {
		return net.JoinHostPort(ip, port)
	}
	return addr
}

func lookupAAAA(ctx context.Context, host string) string {
	rctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if ips, err := net.DefaultResolver.LookupIP(rctx, "ip6", host); err == nil {
		for _, ip := range ips {
			if ip.To4() == nil && ip.To16() != nil {
				return ip.String()
			}
		}
	}
	dohURL := "https://1.1.1.1/dns-query?name=" + url.QueryEscape(host) + "&type=AAAA"
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, dohURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/dns-json")
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var payload struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&payload); err != nil {
		return ""
	}
	for _, ans := range payload.Answer {
		if ans.Type == 28 {
			if parsed, err := netip.ParseAddr(strings.TrimSpace(ans.Data)); err == nil && parsed.Is6() {
				return parsed.String()
			}
		}
	}
	return ""
}

func (p *Pool) markBurnt(prefix string) {
	if prefix == "" {
		return
	}
	now := time.Now()
	expiry := nextResetUTC(now)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneBurntLocked(now)
	if len(p.burnt) >= maxBurntEntries {
		for k := range p.burnt {
			delete(p.burnt, k)
			break
		}
	}
	p.burnt[prefix] = expiry
}

func (p *Pool) isBurnt(prefix string) bool {
	if prefix == "" {
		return false
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	exp, ok := p.burnt[prefix]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(p.burnt, prefix)
		return false
	}
	return true
}

func (p *Pool) pruneBurntLocked(now time.Time) {
	for k, exp := range p.burnt {
		if now.After(exp) {
			delete(p.burnt, k)
		}
	}
}

// nextResetUTC returns the next 01:05 UTC (OpenCode resets daily limits
// between 00:00 and 01:00 UTC).
func nextResetUTC(now time.Time) time.Time {
	u := now.UTC()
	reset := time.Date(u.Year(), u.Month(), u.Day(), 1, 5, 0, 0, time.UTC)
	if !reset.After(u) {
		reset = reset.Add(24 * time.Hour)
	}
	return reset
}

// NormalizePrefix returns the /64 network prefix for IPv6 addresses, or the
// canonical IPv4 string for IPv4 addresses.
func NormalizePrefix(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return p.Masked().String()
}

func fetchTrace(ctx context.Context, client *l4Client) (map[string]string, error) {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	raw, err := client.DialContext(tctx, "[2606:4700:4700::1111]:443")
	if err != nil {
		return nil, err
	}
	defer raw.Close()

	tlsConn := tls.Client(raw, &tls.Config{ServerName: "1.1.1.1", MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(tctx); err != nil {
		return nil, err
	}
	if d, ok := tctx.Deadline(); ok {
		_ = tlsConn.SetDeadline(d)
	}
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, "https://1.1.1.1/cdn-cgi/trace", nil)
	if err != nil {
		return nil, err
	}
	if err := req.Write(tlsConn); err != nil {
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trace HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[k] = v
		}
	}
	return values, nil
}
