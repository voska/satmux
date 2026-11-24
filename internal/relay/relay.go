// Package relay is the ground side: an authenticated HTTP/2 server that opens TCP
// tunnels (CONNECT) and performs whole HTTP requests on the client's behalf (fetch),
// compressing what it sends back across the satellite.
package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/voska/satmux/internal/wire"
)

// Options configure a relay.
type Options struct {
	Token string
	// Quality forces a brotli quality (0-11) for responses. -1 means automatic:
	// calibrate this CPU at startup and pick per response from the client's link rate.
	Quality int
	Levels  []wire.Level // calibration; computed by New when Quality < 0 and nil
	Log     *slog.Logger
	// Transport performs fetches. Nil means a pooled HTTP/1.1+HTTP/2 transport
	// that verifies origin certificates against the system roots.
	Transport http.RoundTripper
	// Dialer opens tunnels. Nil means a dialer with happy eyeballs and a 10 s timeout.
	Dialer *net.Dialer
}

// Relay is an http.Handler serving satmux clients.
type Relay struct {
	opt  Options
	busy atomic.Int64 // responses being compressed right now
}

// New returns a relay handler.
func New(opt Options) *Relay {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Dialer == nil {
		// FallbackDelay: Go's default 300 ms happy-eyeballs wait is tuned for home
		// networks. In a datacenter a connect takes milliseconds, and a VM whose IPv6
		// route is broken would otherwise pay 300 ms on every new origin connection.
		opt.Dialer = &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, FallbackDelay: 50 * time.Millisecond}
	}
	if opt.Transport == nil {
		opt.Transport = &http.Transport{
			Proxy:                 nil,
			DialContext:           opt.Dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       5 * time.Minute, // keep origins warm between page views
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			DisableCompression:    true, // we ask for identity and compress ourselves
		}
	}
	if opt.Quality < 0 && opt.Levels == nil {
		opt.Levels = wire.Calibrate()
		opt.Log.Info("calibrated brotli on this CPU", "levels", wire.FormatLevels(opt.Levels))
	}
	return &Relay{opt: opt}
}

// level picks the brotli quality for one response.
func (s *Relay) level(r *http.Request, size int64) wire.Level {
	if q, err := strconv.Atoi(r.Header.Get(wire.HeaderQuality)); err == nil && q >= 0 && q <= 11 {
		return wire.Level{Quality: q}
	}
	if s.opt.Quality >= 0 {
		return wire.Level{Quality: s.opt.Quality}
	}
	rate, _ := strconv.ParseFloat(r.Header.Get(wire.HeaderLinkRate), 64)
	return wire.Choose(s.opt.Levels, size, rate, int(s.busy.Load())+1)
}

// NewServer returns an HTTP/2-only TLS server for the relay.
func NewServer(addr string, st *State, h http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		Protocols:         &protocols,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       10 * time.Minute,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 1000,
			SendPingTimeout:      20 * time.Second,
			PingTimeout:          15 * time.Second,
		},
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{st.Cert},
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"h2"},
		},
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
}

func (s *Relay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !wire.TokenEqual(r.Header.Get(wire.HeaderToken), s.opt.Token) {
		// Look like nothing in particular to scanners.
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		s.tunnel(w, r)
	case r.Header.Get(wire.HeaderTarget) != "":
		s.fetch(w, r)
	case r.URL.Path == wire.PathPing:
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set(wire.HeaderError, err.Error())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "satmux relay: %v\n", err)
}

func (s *Relay) tunnel(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	conn, err := s.opt.Dialer.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		s.opt.Log.Info("tunnel dial failed", "target", r.Host, "err", err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(r.Context(), func() { _ = conn.Close() })
	defer stop()

	rc := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	go func() {
		// Client -> target. The client may have sent early data before our 200.
		_, _ = io.Copy(conn, r.Body)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	n, _ := wire.Pump(w, conn, rc.Flush, 2*time.Millisecond)
	s.opt.Log.Debug("tunnel closed", "target", r.Host, "down", n, "dur", time.Since(start).Round(time.Millisecond))
}

func (s *Relay) fetch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	target, err := url.Parse(r.Header.Get(wire.HeaderTarget))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		fail(w, http.StatusBadRequest, errors.New("Satmux-Target must be an absolute http or https URL"))
		return
	}

	var body io.Reader = r.Body
	if r.Header.Get(wire.HeaderEncoding) == "br" {
		body = wire.NewDecoder(r.Body)
	}
	length := r.ContentLength
	if v := r.Header.Get(wire.HeaderLength); v != "" {
		length, _ = strconv.ParseInt(v, 10, 64)
	}
	if length == 0 {
		body = nil
	}
	// #nosec G704 -- fetching URLs named by authenticated clients is the relay's job; see README "Security".
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	out.ContentLength = length // keep the origin's Content-Length; many APIs reject chunked uploads
	wire.CopyHeader(out.Header, r.Header)
	out.Header.Set("Accept-Encoding", "identity")

	resp, err := s.opt.Transport.RoundTrip(out)
	if err != nil {
		s.opt.Log.Info("fetch failed", "url", target.Redacted(), "err", err)
		fail(w, http.StatusBadGateway, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	h := w.Header()
	wire.CopyHeader(h, resp.Header)
	hasBody := r.Method != http.MethodHead && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified
	compress := hasBody && resp.Header.Get("Content-Encoding") == "" && !wire.Incompressible(resp.Header.Get("Content-Type"))

	// Sniff the first chunk when the type does not settle it.
	var first []byte
	if compress {
		buf := make([]byte, 32<<10)
		n, rerr := io.ReadAtLeast(resp.Body, buf, 1)
		first = buf[:n]
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			fail(w, http.StatusBadGateway, rerr)
			return
		}
		compress = len(first) > 0 && wire.Compressible(first)
	}

	rc := http.NewResponseController(w)
	if !compress {
		w.WriteHeader(resp.StatusCode)
		if len(first) > 0 {
			_, _ = w.Write(first)
		}
		_ = rc.Flush()
		n, _ := wire.Pump(w, resp.Body, rc.Flush, 5*time.Millisecond)
		s.opt.Log.Debug("fetch", "method", r.Method, "url", target.Redacted(), "status", resp.StatusCode,
			"bytes", n+int64(len(first)), "dur", time.Since(start).Round(time.Millisecond))
		return
	}

	lvl := s.level(r, resp.ContentLength)
	s.busy.Add(1)
	defer s.busy.Add(-1)
	h.Del("Content-Length")
	h.Set(wire.HeaderEncoding, "br")
	h.Set(wire.HeaderQuality, strconv.Itoa(lvl.Quality))
	if lvl.Speed > 0 {
		h.Set(wire.HeaderEncRate, strconv.FormatFloat(lvl.Speed*lvl.Ratio, 'f', 0, 64))
	}
	if resp.ContentLength >= 0 {
		h.Set(wire.HeaderLength, strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
	cw := &countingWriter{w: w}
	enc := wire.NewEncoder(cw, lvl.Quality)
	_, _ = enc.Write(first)
	if enc.Flush() == nil { // the first bytes go out now, not when the next chunk arrives
		_ = rc.Flush()
	}
	n, _ := wire.Pump(enc, resp.Body, func() error {
		if err := enc.Flush(); err != nil {
			return fmt.Errorf("brotli flush: %w", err)
		}
		return rc.Flush() //nolint:wrapcheck // passthrough
	}, 5*time.Millisecond)
	_ = enc.Close()
	_ = rc.Flush()
	s.opt.Log.Debug("fetch", "method", r.Method, "url", target.Redacted(), "status", resp.StatusCode,
		"bytes", n+int64(len(first)), "wire", cw.n, "q", lvl.Quality, "link", r.Header.Get(wire.HeaderLinkRate),
		"dur", time.Since(start).Round(time.Millisecond))
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err //nolint:wrapcheck // passthrough
}
