package client_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voska/satmux/internal/client"
	"github.com/voska/satmux/internal/relay"
	"github.com/voska/satmux/internal/wire"
)

const (
	oneWay = 50 * time.Millisecond // injected latency each way between client and relay
	rtt    = 2 * oneWay
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// delayCopy forwards src to dst, delivering each chunk oneWay after it was read.
func delayCopy(dst, src net.Conn, d time.Duration) {
	type item struct {
		b  []byte
		at time.Time
	}
	ch := make(chan item, 4096)
	go func() {
		defer close(ch)
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			if n > 0 {
				ch <- item{buf[:n], time.Now().Add(d)}
			}
			if err != nil {
				return
			}
		}
	}()
	for it := range ch {
		time.Sleep(time.Until(it.at))
		if _, err := dst.Write(it.b); err != nil {
			break
		}
	}
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// latencyProxy is a TCP proxy to target that adds d of delay in each direction.
func latencyProxy(t *testing.T, target string, d time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				u, err := net.Dial("tcp", target)
				if err != nil {
					_ = c.Close()
					return
				}
				go delayCopy(u, c, d)
				delayCopy(c, u, d)
			}()
		}
	}()
	return ln.Addr().String()
}

type env struct {
	client    *client.Client
	origin    *httptest.Server
	echoAddr  string
	relayAddr string
	state     *relay.State
	relaySrv  *http.Server
	page      []byte
	bin       []byte
}

func startRelay(t *testing.T, addr string, st *relay.State) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := relay.NewServer(ln.Addr().String(), st, relay.New(relay.Options{Token: st.Token, Quality: 5, Log: quiet}))
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return srv, ln.Addr().String()
}

func newEnv(t *testing.T, mutate func(*wire.Endpoint)) *env {
	t.Helper()
	e := &env{
		page: bytes.Repeat([]byte("<li><a href=\"/item?id=1\">A fairly repetitive line of HTML</a></li>\n"), 5000),
		bin:  make([]byte, 1<<20),
	}
	_, _ = rand.Read(e.bin)

	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", fmt.Sprint(len(e.page)))
		_, _ = w.Write(e.page)
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(e.bin)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"len": len(b), "sha": hex.EncodeToString(sum[:]), "content_length": r.ContentLength,
			"chunked": len(r.TransferEncoding) > 0, "auth": r.Header.Get("Authorization"), "ae": r.Header.Get("Accept-Encoding"),
		})
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			_, _ = fmt.Fprintf(w, "data: %d %d\n\n", i, time.Now().UnixMilli())
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/page", http.StatusFound) })
	e.origin = httptest.NewServer(mux)
	t.Cleanup(e.origin.Close)

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	e.echoAddr = echo.Addr().String()

	e.state, err = relay.LoadState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.relaySrv, e.relayAddr = startRelay(t, "127.0.0.1:0", e.state)
	t.Cleanup(func() { _ = e.relaySrv.Close() })

	ep := wire.Endpoint{Addr: latencyProxy(t, e.relayAddr, oneWay), Token: e.state.Token, Pin: e.state.Pin}
	if mutate != nil {
		mutate(&ep)
	}
	e.client = client.New(client.Options{Endpoint: ep, Carriers: 2, HTTPAddr: "127.0.0.1:0", SOCKSAddr: "127.0.0.1:0", Quality: 5, Log: quiet})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) waitUp(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.client.Status().CarriersUp >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("carriers up = %d, want %d", e.client.Status().CarriersUp, n)
}

var direct = &http.Client{Transport: &http.Transport{Proxy: nil, DisableCompression: true}}

func (e *env) gate(path string) string {
	return "http://" + e.client.HTTPAddr() + "/" + e.origin.URL + path
}

func TestGateFetchIsOneRoundTripAndCompressed(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 2)
	start := time.Now()
	resp, err := direct.Get(e.gate("/page"))
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start) // time to first byte: the round-trip count, not CPU (brotli is slow under -race)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(body, e.page) {
		t.Fatalf("body mismatch: got %d bytes", len(body))
	}
	if resp.ContentLength != int64(len(e.page)) {
		t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(e.page))
	}
	if took > rtt*16/10 {
		t.Errorf("gate fetch took %v, want about one round trip (%v)", took, rtt)
	}
	s := e.client.Status()
	if s.WireDown*10 > s.BytesDown {
		t.Errorf("wire bytes %d for %d app bytes: expected >10x compression on repetitive HTML", s.WireDown, s.BytesDown)
	}
}

func TestTunnelOpensWithoutARoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 2)
	c, err := net.Dial("tcp", e.client.HTTPAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	start := time.Now()
	_, _ = fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nhello", e.echoAddr, e.echoAddr)
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT reply %q", line)
	}
	_, _ = br.ReadString('\n') // blank line
	got := make([]byte, 5)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "hello" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if took := time.Since(start); took > rtt*16/10 {
		t.Errorf("first echo after %v, want about one round trip (%v): early data is not pipelined", took, rtt)
	}
}

func TestSOCKS5Tunnel(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 1)
	c, err := net.Dial("tcp", e.client.SOCKSAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	host, port, _ := net.SplitHostPort(e.echoAddr)
	var p int
	_, _ = fmt.Sscan(port, &p)
	req := []byte{5, 1, 0, 5, 1, 0, 3, byte(len(host))}
	req = append(append(req, host...), byte(p>>8), byte(p))
	_, _ = c.Write(append(req, e.bin[:4096]...))
	reply := make([]byte, 2+10)
	if _, err := io.ReadFull(c, reply); err != nil || reply[3] != 0 {
		t.Fatalf("socks reply %v %v", reply, err)
	}
	got := make([]byte, 4096)
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, e.bin[:4096]) {
		t.Fatalf("socks echo mismatch: %v", err)
	}
}

func TestBinaryIntegrityGateAndTunnel(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 1)
	resp, err := direct.Get(e.gate("/bin"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(b, e.bin) {
		t.Fatalf("gate binary mismatch (%d bytes)", len(b))
	}
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustURL("http://" + e.client.HTTPAddr())), DisableCompression: true}}
	// http:// through the proxy takes the gate path too; exercise CONNECT with an https origin.
	tls := httptest.NewTLSServer(e.origin.Config.Handler)
	defer tls.Close()
	tr := tls.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyURL(mustURL("http://" + e.client.HTTPAddr()))
	proxied.Transport = tr
	resp, err = proxied.Get(tls.URL + "/bin")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(b, e.bin) {
		t.Fatalf("tunnel binary mismatch (%d bytes)", len(b))
	}
}

func TestPostBodyKeepsContentLengthAndHeaders(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 1)
	payload := bytes.Repeat([]byte(`{"role":"user","content":"lorem ipsum dolor sit amet"},`), 4000)
	req, _ := http.NewRequest(http.MethodPost, e.gate("/echo"), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer origin-secret")
	resp, err := direct.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	_ = resp.Body.Close()
	sum := sha256.Sum256(payload)
	if got["sha"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("origin saw a different body: %v", got)
	}
	if got["content_length"] != float64(len(payload)) || got["chunked"] != false {
		t.Errorf("origin saw content_length=%v chunked=%v, want %d and false", got["content_length"], got["chunked"], len(payload))
	}
	if got["auth"] != "Bearer origin-secret" {
		t.Errorf("Authorization not forwarded: %v", got["auth"])
	}
	if s := e.client.Status(); s.WireUp*5 > s.BytesUp {
		t.Errorf("request body not compressed: wire %d, app %d", s.WireUp, s.BytesUp)
	}
}

func TestServerSentEventsStream(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 1)
	resp, err := direct.Get(e.gate("/sse"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	var arrivals []time.Time
	for len(arrivals) < 3 {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("after %d events: %v", len(arrivals), err)
		}
		if strings.HasPrefix(line, "data:") {
			arrivals = append(arrivals, time.Now())
		}
	}
	if gap := arrivals[2].Sub(arrivals[0]); gap < 450*time.Millisecond {
		t.Errorf("events arrived %v apart; they were buffered instead of streamed", gap)
	}
}

func TestStatusCodesPassThrough(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 1)
	noRedirect := &http.Client{Transport: direct.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cases := []struct {
		method, path string
		want         int
	}{{"GET", "/redirect", 302}, {"GET", "/missing", 404}, {"HEAD", "/page", 200}}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, e.gate(c.path), nil)
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
		if c.method == "HEAD" && resp.ContentLength != int64(len(e.page)) {
			t.Errorf("HEAD Content-Length = %d", resp.ContentLength)
		}
	}
}

func TestWrongPinNeverConnects(t *testing.T) {
	e := newEnv(t, func(ep *wire.Endpoint) { ep.Pin = "AAAA" })
	time.Sleep(time.Second)
	if up := e.client.Status().CarriersUp; up != 0 {
		t.Fatalf("%d carriers connected to a relay with the wrong key", up)
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	e := newEnv(t, func(ep *wire.Endpoint) { ep.Token = "nope" })
	e.waitUp(t, 1)
	resp, err := direct.Get(e.gate("/page"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || bytes.Contains(b, []byte("fairly repetitive")) {
		t.Fatalf("relay served a request with a bad token: %d", resp.StatusCode)
	}
}

func TestRecoversWhenRelayRestarts(t *testing.T) {
	e := newEnv(t, nil)
	e.waitUp(t, 2)
	_ = e.relaySrv.Close()
	time.Sleep(200 * time.Millisecond)
	srv, _ := startRelay(t, e.relayAddr, e.state)
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.gate("/page"), nil)
	resp, err := direct.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("after relay restart: %d", resp.StatusCode)
	}
}

func TestGateTarget(t *testing.T) {
	cases := map[string]string{
		"/https://example.com/a?b=c": "https://example.com/a?b=c",
		"/https:/example.com/":       "https://example.com/",
		"/http://example.com":        "http://example.com",
		"/HTTPS://example.com/x":     "https://example.com/x",
	}
	for in, want := range cases {
		if got, ok := client.GateTarget(in); !ok || got != want {
			t.Errorf("GateTarget(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"/", "/foo", "/https://", "/ftp://x"} {
		if _, ok := client.GateTarget(bad); ok {
			t.Errorf("GateTarget(%q) accepted", bad)
		}
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
