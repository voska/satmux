package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/voska/satmux/internal/wire"
)

// carrier is one warm HTTP/2 connection to the relay.
type carrier struct {
	id    int
	cc    *http.ClientConn
	rttNS atomic.Int64
	since time.Time
}

// Pool keeps N carriers open, replaces dead ones, and spreads requests over them.
type Pool struct {
	ep  wire.Endpoint
	n   int
	log *slog.Logger
	tr  *http.Transport

	mu       sync.Mutex
	carriers []*carrier
	ready    chan struct{} // closed and replaced whenever a carrier comes up
}

// NewPool prepares a pool; call Run to start it.
func NewPool(ep wire.Endpoint, n int, log *slog.Logger) *Pool {
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		Proxy:       nil,
		DialContext: dialer.DialContext,
		Protocols:   &protocols,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The relay's certificate is self-signed; we authenticate it by key pin instead.
			InsecureSkipVerify: true, // #nosec G402 -- verified by VerifyConnection below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || wire.Pin(cs.PeerCertificates[0]) != ep.Pin {
					return errors.New("relay key does not match the pin in the connection string")
				}
				return nil
			},
		},
		TLSHandshakeTimeout: 20 * time.Second,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 1000,
			SendPingTimeout:      10 * time.Second, // silence for 10 s -> ping
			PingTimeout:          10 * time.Second, // no answer in 10 s -> connection is dead
		},
	}
	return &Pool{ep: ep, n: n, log: log, tr: tr, carriers: make([]*carrier, n), ready: make(chan struct{})}
}

// Run keeps the carriers alive until ctx ends.
func (p *Pool) Run(ctx context.Context) {
	for i := range p.n {
		go p.maintain(ctx, i)
	}
}

func (p *Pool) maintain(ctx context.Context, i int) {
	backoff := time.Second
	for ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cc, err := p.tr.NewClientConn(dctx, "https", p.ep.Addr)
		cancel()
		if err != nil {
			p.log.Warn("carrier connect failed", "carrier", i, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 15*time.Second)
			continue
		}
		backoff = time.Second
		c := &carrier{id: i, cc: cc, since: time.Now()}
		closed := make(chan struct{})
		var once sync.Once
		cc.SetStateHook(func(cc *http.ClientConn) {
			if cc.Err() != nil {
				once.Do(func() { close(closed) })
			}
		})
		p.set(i, c)
		p.log.Info("carrier up", "carrier", i, "up", p.up())
		p.ping(ctx, c)
		ticker := time.NewTicker(20 * time.Second)
	wait:
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				_ = cc.Close()
				return
			case <-closed:
				break wait
			case <-ticker.C:
				p.ping(ctx, c)
			}
		}
		ticker.Stop()
		p.set(i, nil)
		p.log.Warn("carrier down", "carrier", i, "err", cc.Err(), "up", p.up())
	}
}

// ping measures the round trip through a carrier (and keeps middleboxes warm).
func (p *Pool) ping(ctx context.Context, c *carrier) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://satmux"+wire.PathPing, nil)
	req.Header.Set(wire.HeaderToken, p.ep.Token)
	start := time.Now()
	resp, err := c.cc.RoundTrip(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		p.log.Error("relay rejected our token; check the connection string")
		return
	}
	c.rttNS.Store(int64(time.Since(start)))
}

func (p *Pool) set(i int, c *carrier) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.carriers[i] = c
	if c != nil {
		close(p.ready)
		p.ready = make(chan struct{})
	}
}

func (p *Pool) up() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.carriers {
		if c != nil {
			n++
		}
	}
	return n
}

// pick returns the usable carrier with the fewest requests in flight, waiting
// for one to come up if necessary.
func (p *Pool) pick(ctx context.Context, avoid *carrier) (*carrier, error) {
	for {
		p.mu.Lock()
		var best *carrier
		for _, c := range p.carriers {
			if c == nil || c == avoid || c.cc.Err() != nil || c.cc.Available() == 0 {
				continue
			}
			if best == nil || c.cc.InFlight() < best.cc.InFlight() {
				best = c
			}
		}
		ready := p.ready
		p.mu.Unlock()
		if best != nil {
			return best, nil
		}
		if avoid != nil {
			avoid = nil // only one carrier left: better than nothing
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no relay connection: %w", ctx.Err())
		case <-ready:
		}
	}
}

// RoundTrip sends a request to the relay. Requests without a body that fail
// before any response are retried once on a different carrier.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(wire.HeaderToken, p.ep.Token)
	wctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	c, err := p.pick(wctx, nil)
	cancel()
	if err != nil {
		return nil, err
	}
	resp, err := c.cc.RoundTrip(req)
	if err == nil || req.Body != nil || req.Context().Err() != nil {
		return resp, err //nolint:wrapcheck // passthrough
	}
	p.log.Info("retrying on another carrier", "carrier", c.id, "err", err)
	wctx, cancel = context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	c2, perr := p.pick(wctx, c)
	if perr != nil {
		return nil, err //nolint:wrapcheck // passthrough
	}
	return c2.cc.RoundTrip(req) //nolint:wrapcheck // passthrough
}

// CarrierStatus describes one carrier for `satmux status`.
type CarrierStatus struct {
	ID       int     `json:"id"`
	Up       bool    `json:"up"`
	RTTms    float64 `json:"rtt_ms,omitempty"`
	InFlight int     `json:"in_flight"`
	UpSecs   int     `json:"up_secs,omitempty"`
}

// Status reports the carriers.
func (p *Pool) Status() []CarrierStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]CarrierStatus, len(p.carriers))
	for i, c := range p.carriers {
		out[i] = CarrierStatus{ID: i}
		if c == nil {
			continue
		}
		out[i].Up = true
		out[i].RTTms = float64(time.Duration(c.rttNS.Load()).Microseconds()) / 1000
		out[i].InFlight = c.cc.InFlight()
		out[i].UpSecs = int(time.Since(c.since).Seconds())
	}
	return out
}
