package masque

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	cfAPIURL          = "https://api.cloudflareclient.com/v0a4471"
	cfClientVersion   = "a-6.35-4471"
	cfUserAgent       = "WARP for Android"
	l4ConnectSNI      = "consumer-masque-proxy.cloudflareclient.com"
	defaultMASQUEPort = 443
)

// slotConfig holds the persisted MASQUE credentials for one pool slot.
// Compatible with existing usque config.json files.
type slotConfig struct {
	PrivateKey     string `json:"private_key"`
	EndpointV4     string `json:"endpoint_v4"`
	EndpointV6     string `json:"endpoint_v6"`
	EndpointPubKey string `json:"endpoint_pub_key"`
	ID             string `json:"id"`
	AccessToken    string `json:"access_token"`
	IPv4           string `json:"ipv4"`
	IPv6           string `json:"ipv6"`
}

func loadSlotConfig(path string) (slotConfig, error) {
	var cfg slotConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ID == "" || cfg.AccessToken == "" || cfg.PrivateKey == "" || cfg.EndpointV4 == "" {
		return cfg, fmt.Errorf("incomplete slot config in %s", path)
	}
	return cfg, nil
}

func saveSlotConfig(path string, cfg slotConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".slot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

type cfAccountResp struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Config struct {
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

// registerAndEnroll creates a new Cloudflare WARP device and immediately
// enrolls a fresh P-256 MASQUE keypair.
func registerAndEnroll(ctx context.Context) (slotConfig, error) {
	var wgPub [32]byte
	if _, err := rand.Read(wgPub[:]); err != nil {
		return slotConfig{}, err
	}
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return slotConfig{}, err
	}

	regBody := map[string]any{
		"key":        base64.StdEncoding.EncodeToString(wgPub[:]),
		"install_id": "",
		"fcm_token":  "",
		"tos":        time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"model":      "PC",
		"serial_number": fmt.Sprintf("%x", serial[:]),
		"os_version": "",
		"key_type":   "curve25519",
		"tunnel_type": "wireguard",
		"locale":     "en_US",
	}
	var reg cfAccountResp
	if err := cfRequest(ctx, http.MethodPost, cfAPIURL+"/reg", "", regBody, &reg); err != nil {
		return slotConfig{}, fmt.Errorf("register device: %w", err)
	}
	if reg.ID == "" || reg.Token == "" {
		return slotConfig{}, errors.New("register device: empty id or token")
	}

	base := slotConfig{
		ID:          reg.ID,
		AccessToken: reg.Token,
	}
	return enrollNewKey(ctx, base)
}

// enrollNewKey rotates the MASQUE P-256 keypair for an existing device.
func enrollNewKey(ctx context.Context, prev slotConfig) (slotConfig, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return slotConfig{}, fmt.Errorf("generate P-256 key: %w", err)
	}
	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return slotConfig{}, fmt.Errorf("marshal private key: %w", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		return slotConfig{}, fmt.Errorf("marshal public key: %w", err)
	}

	body := map[string]any{
		"key":         base64.StdEncoding.EncodeToString(pubDER),
		"key_type":    "secp256r1",
		"tunnel_type": "masque",
	}
	var resp cfAccountResp
	endpoint := fmt.Sprintf("%s/reg/%s", cfAPIURL, prev.ID)
	if err := cfRequest(ctx, http.MethodPatch, endpoint, prev.AccessToken, body, &resp); err != nil {
		return slotConfig{}, fmt.Errorf("enroll key: %w", err)
	}
	if len(resp.Config.Peers) == 0 {
		return slotConfig{}, errors.New("enroll key: no peers in response")
	}
	peer := resp.Config.Peers[0]
	return slotConfig{
		PrivateKey:     base64.StdEncoding.EncodeToString(privDER),
		EndpointV4:     stripPort(peer.Endpoint.V4),
		EndpointV6:     stripPort(peer.Endpoint.V6),
		EndpointPubKey: peer.PublicKey,
		ID:             resp.ID,
		AccessToken:    prev.AccessToken,
		IPv4:           resp.Config.Interface.Addresses.V4,
		IPv6:           resp.Config.Interface.Addresses.V6,
	}, nil
}

func cfRequest(ctx context.Context, method, rawURL, token string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", cfUserAgent)
	req.Header.Set("CF-Client-Version", cfClientVersion)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloudflare API %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode cloudflare API response: %w", err)
		}
	}
	return nil
}

func stripPort(raw string) string {
	raw = strings.TrimSpace(raw)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host
	}
	return strings.Trim(raw, "[]")
}

func (c slotConfig) ecPrivateKey() (*ecdsa.PrivateKey, error) {
	der, err := base64.StdEncoding.DecodeString(c.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	return x509.ParseECPrivateKey(der)
}

func (c slotConfig) peerPublicKey() (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(c.EndpointPubKey))
	var der []byte
	if block != nil {
		der = block.Bytes
	} else {
		var err error
		der, err = base64.StdEncoding.DecodeString(strings.TrimSpace(c.EndpointPubKey))
		if err != nil {
			return nil, fmt.Errorf("decode endpoint public key: %w", err)
		}
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint public key: %w", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("endpoint public key is not ECDSA")
	}
	return ecPub, nil
}

func buildTLSConfig(cfg slotConfig) (*tls.Config, error) {
	priv, err := cfg.ecPrivateKey()
	if err != nil {
		return nil, err
	}
	peerPub, err := cfg.peerPublicKey()
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "kestrel"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create client cert: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certDER},
			PrivateKey:  priv,
		}},
		ServerName:         l4ConnectSNI,
		NextProtos:         []string{http3.NextProtoH3},
		InsecureSkipVerify: true, // SNI is not on the peer cert; we pin peerPub directly below.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("missing peer certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			ecPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
			if !ok {
				return x509.ErrUnsupportedAlgorithm
			}
			if !ecPub.Equal(peerPub) {
				return errors.New("MASQUE endpoint public key mismatch")
			}
			return nil
		},
	}, nil
}

// l4Client manages one persistent HTTP/3 MASQUE connection to Cloudflare WARP
// and opens TCP CONNECT streams over it in-process.
type l4Client struct {
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	endpoint   *net.UDPAddr

	mu         sync.Mutex
	udpConn    *net.UDPConn
	quicConn   *quic.Conn
	clientConn *http3.ClientConn
	closed     bool
}

func newL4Client(cfg slotConfig) (*l4Client, error) {
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(cfg.EndpointV4)
	if ip == nil {
		return nil, fmt.Errorf("invalid MASQUE endpoint IP %q", cfg.EndpointV4)
	}
	return &l4Client{
		tlsConfig: tlsCfg,
		quicConfig: &quic.Config{
			EnableDatagrams: true,
			KeepAlivePeriod: 30 * time.Second,
			MaxIdleTimeout:  2 * time.Minute,
		},
		endpoint: &net.UDPAddr{IP: ip, Port: defaultMASQUEPort},
	}, nil
}

func (c *l4Client) DialContext(ctx context.Context, target string) (net.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := c.dialOnce(ctx, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (c *l4Client) dialOnce(ctx context.Context, target string) (net.Conn, error) {
	h3, localAddr, err := c.ensureConn(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := h3.OpenRequestStream(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.resetConn(h3)
		}
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodConnect, "https://"+target, nil)
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	req.Host = target
	if err := stream.SendRequestHeader(req); err != nil {
		_ = stream.Close()
		if ctx.Err() == nil {
			c.resetConn(h3)
		}
		return nil, err
	}
	resp, err := stream.ReadResponse()
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = stream.Close()
		return nil, fmt.Errorf("MASQUE CONNECT %s rejected: HTTP %d", target, resp.StatusCode)
	}
	return &masqueConn{
		stream: stream,
		local:  localAddr,
		remote: masqueAddr(target),
	}, nil
}

func (c *l4Client) ensureConn(ctx context.Context) (*http3.ClientConn, net.Addr, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	if c.clientConn != nil {
		h3 := c.clientConn
		addr := c.udpConn.LocalAddr()
		c.mu.Unlock()
		return h3, addr, nil
	}
	c.mu.Unlock()

	bindIP := net.IPv4zero
	if c.endpoint.IP.To4() == nil {
		bindIP = net.IPv6zero
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP})
	if err != nil {
		return nil, nil, err
	}
	// ConnectionIDLength=20 avoids Cloudflare MASQUE PROTOCOL_VIOLATION disconnects.
	tr := &quic.Transport{Conn: udpConn, ConnectionIDLength: 20}
	quicConn, err := tr.Dial(ctx, c.endpoint, c.tlsConfig, c.quicConfig)
	if err != nil {
		_ = udpConn.Close()
		return nil, nil, err
	}
	h3 := (&http3.Transport{}).NewClientConn(quicConn)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = quicConn.CloseWithError(0, "closed")
		_ = udpConn.Close()
		return nil, nil, net.ErrClosed
	}
	if c.clientConn != nil {
		existing := c.clientConn
		addr := c.udpConn.LocalAddr()
		c.mu.Unlock()
		_ = quicConn.CloseWithError(0, "duplicate")
		_ = udpConn.Close()
		return existing, addr, nil
	}
	c.udpConn = udpConn
	c.quicConn = quicConn
	c.clientConn = h3
	addr := udpConn.LocalAddr()
	c.mu.Unlock()
	return h3, addr, nil
}

func (c *l4Client) resetConn(expected *http3.ClientConn) {
	c.mu.Lock()
	if c.clientConn != expected {
		c.mu.Unlock()
		return
	}
	qc := c.quicConn
	uc := c.udpConn
	c.clientConn = nil
	c.quicConn = nil
	c.udpConn = nil
	c.mu.Unlock()

	if qc != nil {
		_ = qc.CloseWithError(0, "reset")
	}
	if uc != nil {
		_ = uc.Close()
	}
}

func (c *l4Client) Close() error {
	c.mu.Lock()
	c.closed = true
	qc := c.quicConn
	uc := c.udpConn
	c.clientConn = nil
	c.quicConn = nil
	c.udpConn = nil
	c.mu.Unlock()

	if qc != nil {
		_ = qc.CloseWithError(0, "shutdown")
	}
	if uc != nil {
		_ = uc.Close()
	}
	return nil
}

type masqueConn struct {
	stream *http3.RequestStream
	local  net.Addr
	remote net.Addr
	once   sync.Once
}

func (m *masqueConn) Read(p []byte) (int, error)  { return m.stream.Read(p) }
func (m *masqueConn) Write(p []byte) (int, error) { return m.stream.Write(p) }
func (m *masqueConn) Close() error {
	var err error
	m.once.Do(func() {
		err = m.stream.Close()
		m.stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
	})
	return err
}
func (m *masqueConn) LocalAddr() net.Addr                { return m.local }
func (m *masqueConn) RemoteAddr() net.Addr               { return m.remote }
func (m *masqueConn) SetDeadline(t time.Time) error      { return m.stream.SetDeadline(t) }
func (m *masqueConn) SetReadDeadline(t time.Time) error  { return m.stream.SetReadDeadline(t) }
func (m *masqueConn) SetWriteDeadline(t time.Time) error { return m.stream.SetWriteDeadline(t) }

type masqueAddr string

func (a masqueAddr) Network() string { return "masque-l4-tcp" }
func (a masqueAddr) String() string  { return string(a) }
