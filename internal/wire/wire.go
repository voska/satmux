// Package wire holds what the client and the relay must agree on: the connection
// string, key pinning, header names and the body codec.
package wire

import (
	"compress/flate"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
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

	"github.com/andybalholm/brotli"
)

// Headers carried between client and relay. Everything else is the user's request.
const (
	HeaderToken    = "Satmux-Token"        //nolint:gosec // a header name, not a credential; carries the shared secret
	HeaderTarget   = "Satmux-Target"       // absolute URL for a fetch
	HeaderEncoding = "Satmux-Encoding"     // "br" when satmux (not the origin) brotli-encoded the body
	HeaderLength   = "Satmux-Length"       // decoded body length, when known
	HeaderError    = "Satmux-Error"        // why the relay could not complete the request
	HeaderQuality  = "Satmux-Quality"      // request: force a brotli quality (0-11); response: the quality used
	HeaderLinkRate = "Satmux-Link-Rate"    // request: the client's measured link throughput, bytes/s
	HeaderEncRate  = "Satmux-Encoder-Rate" // response: bytes/s the relay's encoder can produce at that quality
	PathPing       = "/_satmux/ping"
	Scheme         = "satmux"
	DefaultPort    = 8443
)

// Endpoint is everything a client needs to reach and authenticate a relay.
// Its string form is satmux://TOKEN@HOST:PORT?pin=SPKI_SHA256.
type Endpoint struct {
	Addr  string // host:port
	Token string
	Pin   string // base64url SHA-256 of the relay's SubjectPublicKeyInfo
}

func (e Endpoint) String() string {
	return (&url.URL{Scheme: Scheme, User: url.User(e.Token), Host: e.Addr, RawQuery: "pin=" + e.Pin}).String()
}

// ParseEndpoint parses a connection string printed by `satmux relay invite`.
func ParseEndpoint(s string) (Endpoint, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse connection string: %w", err)
	}
	if u.Scheme != Scheme || u.User == nil || u.Host == "" {
		return Endpoint{}, errors.New("connection string must look like satmux://TOKEN@HOST:PORT?pin=PIN")
	}
	e := Endpoint{Addr: u.Host, Token: u.User.Username(), Pin: u.Query().Get("pin")}
	if u.Port() == "" {
		e.Addr = net.JoinHostPort(u.Hostname(), strconv.Itoa(DefaultPort))
	}
	if e.Token == "" || e.Pin == "" {
		return Endpoint{}, errors.New("connection string is missing the token or the pin")
	}
	return e, nil
}

// Pin returns the SPKI pin of a certificate.
func Pin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TokenEqual compares tokens in constant time.
func TokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// CopyHeader copies end-to-end headers from src to dst, dropping hop-by-hop headers
// (including those named in Connection) and satmux's own headers.
func CopyHeader(dst, src http.Header) {
	drop := map[string]bool{}
	for _, h := range hopByHop {
		drop[h] = true
	}
	for _, v := range src.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			drop[http.CanonicalHeaderKey(strings.TrimSpace(f))] = true
		}
	}
	for k, vs := range src {
		if drop[k] || strings.HasPrefix(k, "Satmux-") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// Incompressible reports content types that are already compressed.
func Incompressible(contentType string) bool {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "image/") && !strings.Contains(ct, "svg") && !strings.Contains(ct, "bmp"),
		strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"),
		strings.HasPrefix(ct, "font/woff"),
		strings.Contains(ct, "zip"), strings.Contains(ct, "gzip"), strings.Contains(ct, "x-xz"),
		strings.Contains(ct, "zstd"), strings.Contains(ct, "x-7z"), strings.Contains(ct, "x-rar"),
		strings.Contains(ct, "x-bzip"), strings.Contains(ct, "octet-stream"):
		return true
	}
	return false
}

// Compressible is a cheap check on a sample: does fast deflate shrink it by 10%?
func Compressible(sample []byte) bool {
	if len(sample) > 32<<10 {
		sample = sample[:32<<10]
	}
	if len(sample) < 512 {
		return true
	}
	var n countWriter
	w, _ := flate.NewWriter(&n, flate.BestSpeed)
	_, _ = w.Write(sample)
	_ = w.Close()
	return float64(n) < 0.9*float64(len(sample))
}

type countWriter int

func (c *countWriter) Write(p []byte) (int, error) { *c += countWriter(len(p)); return len(p), nil }

// NewEncoder returns a streaming brotli encoder. Quality 11 compresses text about
// 20% smaller than gzip at roughly 1 MB/s; on fast links pass a lower quality.
func NewEncoder(w io.Writer, quality int) *brotli.Writer {
	return brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: quality, LGWin: 22})
}

// NewDecoder returns a streaming brotli decoder.
func NewDecoder(r io.Reader) io.Reader { return brotli.NewReader(r) }

// Pump copies src to dst and calls flush whenever src goes quiet: after a read, if
// no more data arrives within linger, everything written so far is flushed. Bulk
// transfers are batched into big writes; interactive streams (SSE, keystrokes)
// are never held back by more than linger.
func Pump(dst io.Writer, src io.Reader, flush func() error, linger time.Duration) (int64, error) {
	type chunk struct {
		b   []byte
		err error
	}
	ch := make(chan chunk, 8)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			buf := make([]byte, 32<<10)
			n, err := src.Read(buf)
			select {
			case ch <- chunk{buf[:n], err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var total int64
	pending := false
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		var c chunk
		if pending {
			timer.Reset(linger)
			select {
			case c = <-ch:
				timer.Stop()
			case <-timer.C:
				if err := flush(); err != nil {
					return total, err
				}
				pending = false
				c = <-ch
			}
		} else {
			c = <-ch
		}
		if len(c.b) > 0 {
			n, err := dst.Write(c.b)
			total += int64(n)
			if err != nil {
				return total, fmt.Errorf("write: %w", err)
			}
			pending = true
		}
		if c.err != nil {
			if pending {
				if err := flush(); err != nil {
					return total, err
				}
			}
			if errors.Is(c.err, io.EOF) {
				return total, nil
			}
			return total, c.err
		}
	}
}
