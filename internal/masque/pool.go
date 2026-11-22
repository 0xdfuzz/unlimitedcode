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

// recoverBackoffs controls the exponential backoff schedule used to revive a
// failed spare slot after a transient network/DNS outage. Overridable in tests.
var recoverBackoffs = []time.Duration{
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

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

	mu         sync.Mutex
	cfg        slotConfig
	client     *l4Client
	state      string
	ip         string
	prefix     string
	colo       string
	lastErr    string
	recycles   int64
	recovering bool
}

type aaaaEntry struct {
	ip         string
	expires    time.Time
	refreshing bool
}

type Pool struct {
	stateDir  string
	size      int
	timeout   time.Duration
	log       Logger
	recycleFn func(ctx context.Context, s *slot, avoidPrefix string) error

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

	p.loadBurnt()

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

// Start boots all pool slots in parallel and returns as soon as the first slot
// is verified ready so the local proxy accepts traffic immediately. Remaining
// spare slots finish booting and deduplicating their /64 prefix in the background.
func (p *Pool) Start(ctx context.Context) error {
	// Pre-warm IPv6 AAAA records in parallel with slot boot so the first prompt
	// never stalls waiting on cold OS/DoH DNS resolution.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		warmCtx, cancel := context.WithTimeout(p.ctx, 4*time.Second)
		defer cancel()
		_ = p.resolveIPv6Target(warmCtx, "opencode.ai:443")
	}()

	var (
		bootWG     sync.WaitGroup
		firstOnce  sync.Once
		firstReady = make(chan int, 1)
		errMu      sync.Mutex
		bootErrs   []string
	)

	for _, s := range p.slots {
		bootWG.Add(1)
		p.wg.Add(1)
		go func(sl *slot) {
			defer bootWG.Done()
			defer p.wg.Done()

			slotCtx, slotCancel := context.WithTimeout(p.ctx, 45*time.Second)
			err := p.bootSlot(slotCtx, sl)
			slotCancel()
			if err != nil {
				p.log.Printf("masque slot=%d boot failed: %v", sl.id, err)
				errMu.Lock()
				bootErrs = append(bootErrs, fmt.Sprintf("slot %d: %v", sl.id, err))
				errMu.Unlock()
				p.scheduleRecover(sl)
				return
			}

			becameActive := false
			firstOnce.Do(func() {
				p.mu.Lock()
				p.active = sl.id
				p.mu.Unlock()
				becameActive = true
				firstReady <- sl.id
			})
			if becameActive {
				return
			}

			// Spare slot finished: if it landed on the same /64 prefix as the active
			// slot, recycle it in the background so the first rotation is instant.
			active := p.activeSlot()
			active.mu.Lock()
			activePrefix := active.prefix
			active.mu.Unlock()
			sl.mu.Lock()
			dup := activePrefix != "" && sl.state == "ready" && sl.prefix == activePrefix
			sl.mu.Unlock()
			if dup {
				p.log.Printf("masque slot=%d shares prefix %s with active slot=%d; recycling in background", sl.id, activePrefix, active.id)
				bgCtx, bgCancel := context.WithTimeout(p.ctx, 45*time.Second)
				defer bgCancel()
				if err := p.recycleSlot(bgCtx, sl, activePrefix); err != nil {
					p.log.Printf("masque slot=%d dedup recycle failed: %v", sl.id, err)
					p.scheduleRecover(sl)
				}
			}
		}(s)
	}

	allDone := make(chan struct{})
	go func() {
		bootWG.Wait()
		close(allDone)
	}()

	select {
	case activeID := <-firstReady:
		p.log.Printf("masque in-process pool ready active=%d (warming %d total slots)", activeID, p.size)
		return nil
	case <-allDone:
		// Either one slot won firstReady simultaneously or every slot failed.
		select {
		case activeID := <-firstReady:
			p.log.Printf("masque in-process pool ready active=%d (warming %d total slots)", activeID, p.size)
			return nil
		default:
			errMu.Lock()
			summary := strings.Join(bootErrs, "; ")
			errMu.Unlock()
			p.Close()
			return fmt.Errorf("masque pool failed to start any slot: %s", summary)
		}
	case <-ctx.Done():
		p.Close()
		return ctx.Err()
	}
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
	directTransport.CloseIdleConnections()
	dohIPTransport.CloseIdleConnections()
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

		p.mu.Lock()
		if !p.closed {
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				bgCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
				defer cancel()
				if err := p.doRecycle(bgCtx, old, oldPrefix); err != nil {
					p.log.Printf("masque bg recycle slot=%d failed: %v", old.id, err)
					p.scheduleRecover(old)
				}
			}()
		}
		p.mu.Unlock()
		p.healFailedSlots()
		return nil
	}

	p.log.Printf("masque sync-rotate recycling slot=%d (%s)", old.id, oldIP)
	if err := p.doRecycle(ctx, old, oldPrefix); err != nil {
		if alt := p.pickOtherReady(old.id); alt != nil {
			p.mu.Lock()
			p.active = alt.id
			p.mu.Unlock()
			p.scheduleRecover(old)
			return nil
		}
		p.scheduleRecover(old)
		p.healFailedSlots()
		return err
	}
	p.healFailedSlots()
	return nil
}

func (p *Pool) doRecycle(ctx context.Context, s *slot, avoidPrefix string) error {
	if p.recycleFn != nil {
		return p.recycleFn(ctx, s, avoidPrefix)
	}
	return p.recycleSlot(ctx, s, avoidPrefix)
}

// healFailedSlots schedules background recovery for any spare slot stuck in
// "failed" state (e.g. after a transient network/DNS outage).
func (p *Pool) healFailedSlots() {
	activeID := p.activeSlot().id
	for _, s := range p.slots {
		if s.id == activeID {
			continue
		}
		s.mu.Lock()
		failed := s.state == "failed"
		s.mu.Unlock()
		if failed {
			p.scheduleRecover(s)
		}
	}
}

// scheduleRecover launches at most one background goroutine per slot to revive
// a failed slot with exponential backoff. Stops immediately once the slot
// returns to "ready" or the pool closes (0% CPU when healthy).
func (p *Pool) scheduleRecover(s *slot) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	s.mu.Lock()
	if s.state == "ready" || s.recovering {
		s.mu.Unlock()
		p.mu.Unlock()
		return
	}
	s.recovering = true
	s.mu.Unlock()
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		defer func() {
			s.mu.Lock()
			s.recovering = false
			s.mu.Unlock()
		}()

		for i, wait := range recoverBackoffs {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(wait):
			}

			s.mu.Lock()
			alreadyReady := s.state == "ready"
			s.mu.Unlock()
			if alreadyReady {
				return
			}

			active := p.activeSlot()
			active.mu.Lock()
			activePrefix := active.prefix
			active.mu.Unlock()

			tryCtx, cancel := context.WithTimeout(p.ctx, 45*time.Second)
			err := p.doRecycle(tryCtx, s, activePrefix)
			cancel()
			if err == nil {
				p.log.Printf("masque slot=%d recovered on retry %d", s.id, i+1)
				return
			}
			p.log.Printf("masque slot=%d recovery retry %d/%d failed: %v", s.id, i+1, len(recoverBackoffs), err)
		}
	}()
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
	// If sibling sub-agents are still actively streaming on s.client, do not
	// PATCH /reg/<prev.ID> (replacing the device's P-256 key can terminate
	// in-flight QUIC streams on Cloudflare's edge); register a fresh device ID
	// instead so active streams drain cleanly with zero ECONNRESET.
	hasActiveStreams := s.client != nil && s.client.ActiveStreams() > 0
	s.mu.Unlock()

	var lastErr error
	enrollErr := hasActiveStreams
	for attempt := 1; attempt <= maxRecycleTries; attempt++ {
		if ctx.Err() != nil {
			s.setFailed(ctx.Err())
			return ctx.Err()
		}

		var nextCfg slotConfig
		var err error
		// Prefer lightweight key re-enrollment (PATCH /reg/<id>) on the first two
		// tries — it rotates the IPv6 /64 most of the time without hitting
		// Cloudflare's stricter POST /reg rate limit (1015). Fall back to
		// registering a brand-new WARP device on attempt >= 3 or if PATCH fails.
		if attempt <= 2 && !enrollErr && prev.ID != "" && prev.AccessToken != "" {
			nextCfg, err = enrollNewKey(ctx, prev)
			if err != nil {
				enrollErr = true
			}
		} else {
			nextCfg, err = registerAndEnroll(ctx)
		}
		if err != nil {
			lastErr = err
			p.log.Printf("masque slot=%d recycle try=%d enroll error: %v", s.id, attempt, err)
			if isRateLimitErr(err) {
				// Stop immediately on Cloudflare 429/1015 so we don't extend the
				// rate-limit window; background recovery will retry with backoff.
				break
			}
			if attempt < maxRecycleTries {
				select {
				case <-ctx.Done():
					s.setFailed(ctx.Err())
					return ctx.Err()
				case <-time.After(800 * time.Millisecond):
				}
			}
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
	oldClient := s.client
	s.client = client
	s.state = "ready"
	s.ip = ip
	s.prefix = prefix
	s.colo = trace["colo"]
	s.lastErr = ""
	s.mu.Unlock()
	if oldClient != nil && oldClient != client {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = oldClient.Close()
		} else {
			p.wg.Add(1)
			p.mu.Unlock()
			go func(oc *l4Client) {
				defer p.wg.Done()
				oc.DrainAndClose(p.ctx, 3*time.Minute)
			}(oldClient)
		}
	}
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
	if entry, ok := p.aaaaCache[key]; ok {
		if now.Before(entry.expires) {
			p.mu.Unlock()
			if entry.ip != "" {
				return net.JoinHostPort(entry.ip, port)
			}
			return addr
		}
		// Stale-while-revalidate: serve the last known IPv6 immediately (0ms)
		// and refresh asynchronously so prompts never hit a DNS pause.
		if entry.ip != "" {
			if !entry.refreshing && !p.closed {
				entry.refreshing = true
				p.aaaaCache[key] = entry
				p.wg.Add(1)
				go func() {
					defer p.wg.Done()
					rctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
					defer cancel()
					p.refreshAAAA(rctx, key)
				}()
			}
			p.mu.Unlock()
			return net.JoinHostPort(entry.ip, port)
		}
	}
	p.mu.Unlock()

	ip := p.refreshAAAA(ctx, key)
	if ip != "" {
		return net.JoinHostPort(ip, port)
	}
	return addr
}

func (p *Pool) refreshAAAA(ctx context.Context, key string) string {
	ip := lookupAAAA(ctx, key)
	ttl := 30 * time.Minute
	if ip == "" {
		ttl = 60 * time.Second
	}
	now := time.Now()
	p.mu.Lock()
	if len(p.aaaaCache) >= 64 {
		for k, v := range p.aaaaCache {
			if !now.Before(v.expires) {
				delete(p.aaaaCache, k)
			}
		}
	}
	if ip == "" {
		if prev, ok := p.aaaaCache[key]; ok && prev.ip != "" {
			p.aaaaCache[key] = aaaaEntry{ip: prev.ip, expires: now.Add(2 * time.Minute)}
			p.mu.Unlock()
			return prev.ip
		}
	}
	p.aaaaCache[key] = aaaaEntry{ip: ip, expires: now.Add(ttl)}
	p.mu.Unlock()
	return ip
}

func lookupAAAA(ctx context.Context, host string) string {
	// Cap OS DNS lookup to 500ms so IPv4-only routers fail over to DoH quickly.
	osCtx, osCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	ips, err := net.DefaultResolver.LookupIP(osCtx, "ip6", host)
	osCancel()
	if err == nil {
		for _, ip := range ips {
			if ip.To4() == nil && ip.To16() != nil {
				return ip.String()
			}
		}
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dohEndpoints := []string{
		"https://1.1.1.1/dns-query?name=" + url.QueryEscape(host) + "&type=AAAA",
		"https://dns.google/resolve?name=" + url.QueryEscape(host) + "&type=AAAA",
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: directTransport,
	}
	for _, dohURL := range dohEndpoints {
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, dohURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/dns-json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}
		var payload struct {
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			} `json:"Answer"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, ans := range payload.Answer {
			if ans.Type == 28 {
				if parsed, err := netip.ParseAddr(strings.TrimSpace(ans.Data)); err == nil && parsed.Is6() {
					return parsed.String()
				}
			}
		}
	}
	return ""
}

func (p *Pool) burntPath() string {
	return filepath.Join(p.stateDir, "burnt.json")
}

func (p *Pool) loadBurnt() {
	raw, err := os.ReadFile(p.burntPath())
	if err != nil || len(raw) == 0 {
		return
	}
	var saved map[string]int64
	if err := json.Unmarshal(raw, &saved); err != nil {
		return
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, unixSec := range saved {
		exp := time.Unix(unixSec, 0)
		if now.Before(exp) && len(p.burnt) < maxBurntEntries {
			p.burnt[k] = exp
		}
	}
}

func (p *Pool) saveBurntLocked() {
	out := make(map[string]int64, len(p.burnt))
	for k, exp := range p.burnt {
		out[k] = exp.Unix()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(p.stateDir, ".burnt-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, p.burntPath()); err != nil {
		os.Remove(tmpName)
	}
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
	p.saveBurntLocked()
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

// nextResetUTC returns the next 00:01 UTC (OpenCode resets daily IP quotas at
// 00:00 UTC via buildYYYYMMDD(now); +1 min covers minor clock skew).
func nextResetUTC(now time.Time) time.Time {
	u := now.UTC()
	reset := time.Date(u.Year(), u.Month(), u.Day(), 0, 1, 0, 0, time.UTC)
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
	if vals, err := fetchTraceTarget(ctx, client, "[2606:4700:4700::1111]:443"); err == nil {
		return vals, nil
	}
	return fetchTraceTarget(ctx, client, "1.1.1.1:443")
}

func fetchTraceTarget(ctx context.Context, client *l4Client, target string) (map[string]string, error) {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	raw, err := client.DialContext(tctx, target)
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
