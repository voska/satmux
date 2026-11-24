package wire

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"time"
)

// Level is a brotli quality with its measured cost on this machine.
type Level struct {
	Quality int     `json:"quality"`
	Speed   float64 `json:"speed_bps"` // input bytes compressed per second, one core
	Ratio   float64 `json:"ratio"`     // output / input on the calibration corpus
}

// Qualities considered by the tuner. Levels below 4 rarely win; 10 is within a
// hair of 11 at the same cost, so it is skipped.
var Qualities = []int{4, 5, 6, 7, 8, 9, 11}

// Calibrate measures every quality on a web-like corpus. It takes well under a
// second on a small VM and lets satmux trade CPU for bytes on the hardware it
// actually runs on.
func Calibrate() []Level {
	corpus := Corpus(256 << 10)
	levels := make([]Level, 0, len(Qualities))
	for _, q := range Qualities {
		sample := corpus
		if q >= 10 {
			sample = corpus[:64<<10] // slow levels: a smaller sample is enough to time them
		}
		var out bytes.Buffer
		start := time.Now()
		enc := NewEncoder(&out, q)
		for i := 0; i < len(sample); i += 16 << 10 {
			_, _ = enc.Write(sample[i:min(i+16<<10, len(sample))])
			_ = enc.Flush()
		}
		_ = enc.Close()
		el := time.Since(start).Seconds()
		levels = append(levels, Level{Quality: q, Speed: float64(len(sample)) / max(el, 1e-6), Ratio: float64(out.Len()) / float64(len(sample))})
	}
	return levels
}

// Choose returns the quality that should deliver size bytes soonest over a link
// of linkRate bytes/s, when busy other compressions share the CPU. Compression
// and transmission overlap (the encoder streams), so the cost is
// max(compress, transmit) plus the time to compress the first chunk.
// linkRate <= 0 means unknown: assume a fast link, which favours cheap levels
// and never makes a page slower than plain gzip would.
func Choose(levels []Level, size int64, linkRate float64, busy int) Level {
	if linkRate <= 0 {
		linkRate = 2 << 20
	}
	if size <= 0 {
		size = 64 << 10
	}
	share := 1.0
	if cores := runtime.NumCPU(); busy > cores {
		share = float64(cores) / float64(busy)
	}
	best, bestCost := Level{Quality: 5}, -1.0
	for _, l := range levels {
		speed := l.Speed * share
		s := float64(size)
		compress := s / speed
		transmit := s * l.Ratio / linkRate
		first := min(s, 16<<10) / speed
		cost := max(compress, transmit) + first
		if bestCost < 0 || cost < bestCost {
			best, bestCost = l, cost
		}
	}
	return best
}

// FormatLevels renders a calibration table for logs.
func FormatLevels(levels []Level) string {
	var b strings.Builder
	for i, l := range levels {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "q%d=%.1fMB/s@%.3f", l.Quality, l.Speed/(1<<20), l.Ratio)
	}
	return b.String()
}

// Corpus generates n bytes of deterministic HTML and JSON with realistic
// redundancy: repeated markup and keys, varied text and numbers.
func Corpus(n int) []byte {
	words := strings.Fields(`the of and to in is for on that with as by this from at are be or an was it
		not you your all can more new about page search home news data user api time view list item
		comment points ago share reply login account settings privacy terms help docs release version
		repository issue pull request merge branch commit build test deploy server client network
		latency satellite orbit relay compression stream window protocol header response request`)
	r := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- deterministic corpus, not crypto
	sentence := func(k int) string {
		var s []string
		for range k {
			s = append(s, words[r.IntN(len(words))])
		}
		return strings.Join(s, " ")
	}
	var b bytes.Buffer
	b.WriteString("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\"><title>Hacker News</title>" +
		"<link rel=\"stylesheet\" href=\"/static/site.css?v=8a7f\"></head><body><table id=\"hnmain\">\n")
	for i := 0; b.Len() < n/2; i++ {
		fmt.Fprintf(&b, "<tr class=\"athing\" id=\"%d\"><td class=\"title\"><span class=\"rank\">%d.</span>"+
			"<a href=\"https://example%d.com/%s\" class=\"storylink\">%s</a></td></tr>\n"+
			"<tr><td class=\"subtext\"><span class=\"score\" id=\"score_%d\">%d points</span> by <a href=\"user?id=u%d\" class=\"hnuser\">u%d</a> "+
			"<span class=\"age\" title=\"2026-11-%02dT%02d:%02d:00\">%d hours ago</span> | <a href=\"item?id=%d\">%d&nbsp;comments</a></td></tr>\n",
			40000000+i*7, i+1, r.IntN(500), strings.ReplaceAll(sentence(3), " ", "-"), sentence(4+r.IntN(8)),
			40000000+i*7, r.IntN(900), r.IntN(9999), r.IntN(9999), 1+r.IntN(28), r.IntN(24), r.IntN(60), 1+r.IntN(23), 40000000+i*7, r.IntN(400))
	}
	b.WriteString("</table></body></html>\n[")
	for b.Len() < n {
		fmt.Fprintf(&b, `{"id":%d,"name":"%s","version":"%d.%d.%d","description":"%s","downloads":%d,"yanked":false,"requires_python":">=3.%d"},`+"\n",
			r.IntN(1e7), strings.ReplaceAll(sentence(2), " ", "-"), r.IntN(5), r.IntN(30), r.IntN(20), sentence(6+r.IntN(10)), r.IntN(1e6), 8+r.IntN(6))
	}
	return b.Bytes()[:n]
}
