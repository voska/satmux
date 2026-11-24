package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// DefaultBenchURLs are small, stable pages of different shapes.
var DefaultBenchURLs = []string{
	"https://news.ycombinator.com/",
	"https://api.github.com/repos/golang/go",
	"https://pypi.org/pypi/requests/json",
	"https://en.wikipedia.org/wiki/Geostationary_orbit",
	"https://developer.mozilla.org/en-US/",
}

// BenchCmd times URLs three ways.
type BenchCmd struct {
	HTTP    string        `default:"127.0.0.1:8080" env:"SATMUX_HTTP" help:"satmux HTTP proxy address."`
	Timeout time.Duration `default:"60s" help:"Per-request timeout."`
	URLs    []string      `arg:"" name:"url" optional:"" help:"URLs to fetch (default: a few public pages)."`
}

// BenchResult is one URL's timings.
type BenchResult struct {
	URL       string  `json:"url"`
	DirectS   float64 `json:"direct_s"`
	TunnelS   float64 `json:"tunnel_s"`
	GateS     float64 `json:"gate_s"`
	Bytes     int64   `json:"bytes"`
	GateWireB int64   `json:"gate_wire_bytes"`
	DirectErr string  `json:"direct_error,omitempty"`
	TunnelErr string  `json:"tunnel_error,omitempty"`
	GateErr   string  `json:"gate_error,omitempty"`
}

func timeGet(ctx context.Context, hc *http.Client, u string) (float64, int64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (satmux bench)")
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, err //nolint:wrapcheck // shown to the user as is
	}
	n, err := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return time.Since(start).Seconds(), n, err //nolint:wrapcheck // shown to the user as is
}

// Run benchmarks. Each request uses a fresh connection, as a cold page load would.
func (b *BenchCmd) Run(g *Globals) error {
	if _, err := fetchStatus(b.HTTP); err != nil {
		return err
	}
	urls := b.URLs
	if len(urls) == 0 {
		urls = DefaultBenchURLs
	}
	proxyURL, _ := url.Parse("http://" + b.HTTP)
	direct := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	tunnel := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}}
	gate := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}

	var results []BenchResult
	if !g.JSON {
		fmt.Fprintf(os.Stderr, "%-48s %9s %9s %9s %10s\n", "url", "direct", "tunnel", "gate", "gate wire")
	}
	for _, u := range urls {
		r := BenchResult{URL: u}
		run := func(hc *http.Client, target string) (float64, int64, string) {
			ctx, cancel := context.WithTimeout(context.Background(), b.Timeout)
			defer cancel()
			s, n, err := timeGet(ctx, hc, target)
			if err != nil {
				return s, n, err.Error()
			}
			return s, n, ""
		}
		r.DirectS, _, r.DirectErr = run(direct, u)
		r.TunnelS, _, r.TunnelErr = run(tunnel, u)
		before, _ := fetchStatus(b.HTTP)
		r.GateS, r.Bytes, r.GateErr = run(gate, "http://"+b.HTTP+"/"+u)
		after, _ := fetchStatus(b.HTTP)
		r.GateWireB = after.WireDown - before.WireDown
		results = append(results, r)
		if !g.JSON {
			cell := func(s float64, e string) string {
				if e != "" {
					return "error"
				}
				return fmt.Sprintf("%.2fs", s)
			}
			fmt.Printf("%-48s %9s %9s %9s %10s\n", trunc(u, 48), cell(r.DirectS, r.DirectErr), cell(r.TunnelS, r.TunnelErr),
				cell(r.GateS, r.GateErr), human(r.GateWireB))
		}
	}
	if g.JSON {
		return printJSON(results)
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
