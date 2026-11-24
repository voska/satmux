// Package client is the laptop side: local HTTP proxy, gate and SOCKS5 listeners
// that multiplex everything over the carrier pool to the relay.
package client

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voska/satmux/internal/wire"
)

// PathStatus serves the client's status as JSON on the HTTP listener.
const PathStatus = "/_satmux/status"

// Options configure a client.
type Options struct {
	Endpoint  wire.Endpoint
	Carriers  int
	HTTPAddr  string // HTTP proxy + gate, e.g. 127.0.0.1:8080
	SOCKSAddr string // SOCKS5, e.g. 127.0.0.1:1080; empty disables it
	// Quality forces a brotli quality (0-11) both ways. -1 means automatic: the
	// client measures the link and the relay picks levels its CPU can afford.
	Quality int
	Log     *slog.Logger
}

// Client runs the local listeners.
type Client struct {
	opt     Options
	pool    *Pool
	started time.Time
	stats   stats
	link    linkEstimator
	levels  atomic.Pointer[[]wire.Level] // this machine's calibration, for request bodies
	httpLn  net.Listener
	socksLn net.Listener
}

type stats struct {
	fetches, tunnels, errors         atomic.Int64
	wireDown, appDown, wireUp, appUp atomic.Int64
}

// New returns a client.
func New(opt Options) *Client {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Client{opt: opt, pool: NewPool(opt.Endpoint, opt.Carriers, opt.Log)}
}

// Start binds the listeners, starts the pool and serves in the background until ctx ends.
func (c *Client) Start(ctx context.Context) error {
	var err error
	if c.httpLn, err = net.Listen("tcp", c.opt.HTTPAddr); err != nil {
		return fmt.Errorf("listen %s: %w", c.opt.HTTPAddr, err)
	}
	if c.opt.SOCKSAddr != "" {
		if c.socksLn, err = net.Listen("tcp", c.opt.SOCKSAddr); err != nil {
			_ = c.httpLn.Close()
			return fmt.Errorf("listen %s: %w", c.opt.SOCKSAddr, err)
		}
		go c.serveSOCKS()
	}
	c.started = time.Now()
	if c.opt.Quality < 0 {
		go func() { l := wire.Calibrate(); c.levels.Store(&l) }()
	}
	c.pool.Run(ctx)
	srv := &http.Server{Handler: c, ReadHeaderTimeout: time.Minute, ErrorLog: slog.NewLogLogger(c.opt.Log.Handler(), slog.LevelDebug)}
	go func() { _ = srv.Serve(c.httpLn) }()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		if c.socksLn != nil {
			_ = c.socksLn.Close()
		}
	}()
	return nil
}

// HTTPAddr is the bound HTTP proxy address.
func (c *Client) HTTPAddr() string { return c.httpLn.Addr().String() }

// SOCKSAddr is the bound SOCKS5 address, or "".
func (c *Client) SOCKSAddr() string {
	if c.socksLn == nil {
		return ""
	}
	return c.socksLn.Addr().String()
}

// Pool exposes the carrier pool (for status and tests).
func (c *Client) Pool() *Pool { return c.pool }

func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		c.connect(w, r)
	case r.URL.IsAbs():
		c.fetch(w, r, r.URL.String())
	case r.URL.Path == PathStatus:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status())
	default:
		target, ok := GateTarget(r.RequestURI)
		if !ok {
			http.Error(w, "satmux: use this address as an HTTP proxy, or fetch http://"+r.Host+"/https://example.com/path", http.StatusBadRequest)
			return
		}
		c.fetch(w, r, target)
	}
}

// GateTarget extracts the absolute URL from a gate request URI like
// /https://example.com/path?q. Tolerates clients that collapse "//" to "/".
func GateTarget(requestURI string) (string, bool) {
	s := strings.TrimLeft(requestURI, "/")
	for _, scheme := range []string{"https:", "http:"} {
		if len(s) > len(scheme) && strings.EqualFold(s[:len(scheme)], scheme) {
			rest := strings.TrimLeft(s[len(scheme):], "/")
			if rest == "" {
				return "", false
			}
			u, err := url.Parse(strings.ToLower(scheme) + "//" + rest)
			if err != nil || u.Host == "" {
				return "", false
			}
			return u.String(), true
		}
	}
	return "", false
}

// fetch sends a whole HTTP request through the relay: one round trip, compressed both ways.
func (c *Client) fetch(w http.ResponseWriter, r *http.Request, target string) {
	c.stats.fetches.Add(1)
	if r.Header.Get("Upgrade") != "" {
		http.Error(w, "satmux: protocol upgrades are not supported by the gate; use CONNECT", http.StatusNotImplemented)
		return
	}
	hdr := http.Header{}
	wire.CopyHeader(hdr, r.Header)
	hdr.Set(wire.HeaderTarget, target)
	if c.opt.Quality >= 0 {
		hdr.Set(wire.HeaderQuality, strconv.Itoa(c.opt.Quality))
	} else if rate := c.link.get(); rate > 0 {
		hdr.Set(wire.HeaderLinkRate, strconv.FormatFloat(rate, 'f', 0, 64))
	}

	var body io.Reader
	length := int64(0)
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		length = r.ContentLength
		if r.Header.Get("Content-Encoding") == "" && !wire.Incompressible(r.Header.Get("Content-Type")) {
			pr, pw := io.Pipe()
			go func() {
				enc := wire.NewEncoder(&counter{w: pw, n: &c.stats.wireUp}, c.bodyQuality(r.ContentLength))
				n, err := io.Copy(enc, r.Body)
				c.stats.appUp.Add(n)
				if cerr := enc.Close(); err == nil {
					err = cerr
				}
				_ = pw.CloseWithError(err)
			}()
			body = pr
			hdr.Set(wire.HeaderEncoding, "br")
			if r.ContentLength > 0 {
				hdr.Set(wire.HeaderLength, strconv.FormatInt(r.ContentLength, 10))
			}
			length = -1
		} else {
			body = &counter{r: r.Body, n: &c.stats.wireUp}
		}
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, "https://satmux/", body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out.Header = hdr
	out.ContentLength = length
	if body == nil {
		out.Body = nil
	}

	resp, err := c.pool.RoundTrip(out)
	if err != nil {
		c.stats.errors.Add(1)
		c.opt.Log.Warn("fetch failed", "url", target, "err", err)
		http.Error(w, "satmux: relay unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if e := resp.Header.Get(wire.HeaderError); e != "" {
		c.stats.errors.Add(1)
		c.opt.Log.Info("relay could not fetch", "url", target, "err", e)
	}

	wire.CopyHeader(w.Header(), resp.Header)
	m := &meter{r: resp.Body, n: &c.stats.wireDown}
	var src io.Reader = m
	if resp.Header.Get(wire.HeaderEncoding) == "br" {
		src = wire.NewDecoder(src)
		if l := resp.Header.Get(wire.HeaderLength); l != "" {
			w.Header().Set("Content-Length", l)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, err := wire.Pump(w, src, http.NewResponseController(w).Flush, time.Millisecond)
	c.stats.appDown.Add(n)
	encRate, _ := strconv.ParseFloat(resp.Header.Get(wire.HeaderEncRate), 64)
	c.link.observe(m.bytes, m.last.Sub(m.first), encRate, false)
	if err != nil && r.Context().Err() == nil {
		c.opt.Log.Info("fetch interrupted", "url", target, "err", err)
	}
}

// connect handles an HTTP CONNECT by tunnelling through the relay.
func (c *Client) connect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	// Answer at once: the application's first bytes (a TLS ClientHello) then ride
	// in the same burst as the tunnel request instead of waiting a round trip.
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = conn.Close()
		return
	}
	var src io.Reader = conn
	if n := brw.Reader.Buffered(); n > 0 {
		src = io.MultiReader(io.LimitReader(brw.Reader, int64(n)), conn)
	}
	c.tunnel(r.Host, conn, src)
}

// tunnel relays a local connection to target through the relay.
func (c *Client) tunnel(target string, conn net.Conn, src io.Reader) {
	c.stats.tunnels.Add(1)
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	upDone := make(chan struct{})
	go func() {
		// Tunnel bytes are opaque (usually TLS), so app bytes == wire bytes.
		_, err := io.Copy(pw, &counter{r: src, n: &c.stats.appUp, n2: &c.stats.wireUp})
		_ = pw.CloseWithError(err)
		close(upDone)
	}()

	req := (&http.Request{
		Method:        http.MethodConnect,
		URL:           &url.URL{Host: target},
		Host:          target,
		Header:        http.Header{},
		Body:          pr,
		ContentLength: -1,
	}).WithContext(ctx)
	resp, err := c.pool.RoundTrip(req)
	if err != nil {
		c.stats.errors.Add(1)
		c.opt.Log.Info("tunnel failed", "target", target, "err", err)
		_ = pr.CloseWithError(err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		c.stats.errors.Add(1)
		c.opt.Log.Info("tunnel refused", "target", target, "status", resp.StatusCode, "err", resp.Header.Get(wire.HeaderError))
		_ = pr.CloseWithError(errors.New("tunnel refused"))
		return
	}
	m := &meter{r: resp.Body, n: &c.stats.appDown, n2: &c.stats.wireDown}
	_, _ = io.Copy(conn, m)
	// The application may pace a tunnel, so it can only show the link is at least this fast.
	c.link.observe(m.bytes, m.last.Sub(m.first), 0, true)
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	select { // let the other direction finish, but not forever
	case <-upDone:
	case <-time.After(30 * time.Second):
	}
}

func (c *Client) serveSOCKS() {
	for {
		conn, err := c.socksLn.Accept()
		if err != nil {
			return
		}
		go c.socks(conn)
	}
}

// socks implements the SOCKS5 CONNECT command without authentication.
func (c *Client) socks(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	target, err := socksHandshake(br, conn)
	if err != nil {
		c.opt.Log.Debug("socks handshake", "err", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	// Optimistic success, for the same reason as CONNECT.
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = conn.Close()
		return
	}
	c.tunnel(target, conn, br)
}

func socksHandshake(br *bufio.Reader, w io.Writer) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return "", fmt.Errorf("read greeting: %w", err)
	}
	if hdr[0] != 5 {
		return "", errors.New("not SOCKS5")
	}
	if _, err := br.Discard(int(hdr[1])); err != nil {
		return "", fmt.Errorf("read methods: %w", err)
	}
	if _, err := w.Write([]byte{5, 0}); err != nil {
		return "", fmt.Errorf("write method: %w", err)
	}
	var req [4]byte
	if _, err := io.ReadFull(br, req[:]); err != nil {
		return "", fmt.Errorf("read request: %w", err)
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make(net.IP, map[byte]int{1: 4, 4: 16}[req[3]])
		if _, err := io.ReadFull(br, ip); err != nil {
			return "", fmt.Errorf("read address: %w", err)
		}
		host = ip.String()
	case 3:
		n, err := br.ReadByte()
		if err != nil {
			return "", fmt.Errorf("read name length: %w", err)
		}
		name := make([]byte, n)
		if _, err := io.ReadFull(br, name); err != nil {
			return "", fmt.Errorf("read name: %w", err)
		}
		host = string(name)
	default:
		return "", errors.New("unsupported address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(br, port[:]); err != nil {
		return "", fmt.Errorf("read port: %w", err)
	}
	if req[1] != 1 { // only CONNECT
		_, _ = w.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", errors.New("unsupported SOCKS command")
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// Status is the client's state as served on PathStatus.
type Status struct {
	Relay        string          `json:"relay"`
	UptimeSecs   int             `json:"uptime_secs"`
	CarriersUp   int             `json:"carriers_up"`
	Carriers     []CarrierStatus `json:"carriers"`
	Fetches      int64           `json:"fetches"`
	Tunnels      int64           `json:"tunnels"`
	Errors       int64           `json:"errors"`
	BytesDown    int64           `json:"bytes_down"`      // delivered to applications
	WireDown     int64           `json:"wire_bytes_down"` // received from the relay
	BytesUp      int64           `json:"bytes_up"`
	WireUp       int64           `json:"wire_bytes_up"`
	SavedPercent float64         `json:"saved_percent"`
	LinkRate     float64         `json:"link_bytes_per_sec"` // measured; 0 until enough traffic
	Quality      string          `json:"quality"`            // "auto" or the forced level
}

// Status reports counters and carriers.
func (c *Client) Status() Status {
	s := Status{
		Relay:      c.opt.Endpoint.Addr,
		UptimeSecs: int(time.Since(c.started).Seconds()),
		Carriers:   c.pool.Status(),
		Fetches:    c.stats.fetches.Load(),
		Tunnels:    c.stats.tunnels.Load(),
		Errors:     c.stats.errors.Load(),
		BytesDown:  c.stats.appDown.Load(),
		WireDown:   c.stats.wireDown.Load(),
		BytesUp:    c.stats.appUp.Load(),
		WireUp:     c.stats.wireUp.Load(),
	}
	s.LinkRate = c.link.get()
	s.Quality = "auto"
	if c.opt.Quality >= 0 {
		s.Quality = strconv.Itoa(c.opt.Quality)
	}
	for _, cs := range s.Carriers {
		if cs.Up {
			s.CarriersUp++
		}
	}
	if app := s.BytesDown + s.BytesUp; app > 0 {
		s.SavedPercent = 100 * (1 - float64(s.WireDown+s.WireUp)/float64(app))
	}
	return s
}

// counter counts bytes passing through a reader or a writer, as they pass.
type counter struct {
	r     io.Reader
	w     io.Writer
	n, n2 *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	if c.n2 != nil {
		c.n2.Add(int64(n))
	}
	return n, err //nolint:wrapcheck // passthrough
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err //nolint:wrapcheck // passthrough
}

// bodyQuality picks the brotli quality for an upload of size bytes.
func (c *Client) bodyQuality(size int64) int {
	if c.opt.Quality >= 0 {
		return c.opt.Quality
	}
	levels := c.levels.Load()
	if levels == nil {
		return 5 // calibration still running
	}
	return wire.Choose(*levels, size, c.link.get(), 1).Quality
}

// meter counts bytes and records when the first and last arrived.
type meter struct {
	r           io.Reader
	n, n2       *atomic.Int64
	bytes       int64
	first, last time.Time
}

func (m *meter) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		now := time.Now()
		if m.bytes == 0 {
			m.first = now
		}
		m.last = now
		m.bytes += int64(n)
		m.n.Add(int64(n))
		if m.n2 != nil {
			m.n2.Add(int64(n))
		}
	}
	return n, err //nolint:wrapcheck // passthrough
}

// linkEstimator tracks how fast the link delivers bytes to us.
type linkEstimator struct {
	mu   sync.Mutex
	rate float64 // bytes/s, 0 = unknown
}

func (e *linkEstimator) get() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rate
}

// observe records a transfer of n bytes over d. If something other than the link
// may have set the pace (the relay's encoder, or the application in a tunnel),
// the sample only proves the link is at least that fast.
func (e *linkEstimator) observe(n int64, d time.Duration, encoderRate float64, lowerBoundOnly bool) {
	if n < 16<<10 || d < 20*time.Millisecond {
		return // too small to time: it arrived in one burst
	}
	r := float64(n) / d.Seconds()
	e.mu.Lock()
	defer e.mu.Unlock()
	if lowerBoundOnly || (encoderRate > 0 && r > 0.7*encoderRate) {
		e.rate = max(e.rate, r)
		return
	}
	if e.rate == 0 {
		e.rate = r
	} else {
		e.rate = 0.7*e.rate + 0.3*r
	}
}
