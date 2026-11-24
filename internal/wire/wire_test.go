package wire

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestEndpointRoundTrip(t *testing.T) {
	in := Endpoint{Addr: "203.0.113.7:8443", Token: "tok_EN-123", Pin: "abc-DEF_09"}
	out, err := ParseEndpoint(in.String())
	if err != nil || out != in {
		t.Fatalf("round trip: %+v, %v", out, err)
	}
	def, err := ParseEndpoint("satmux://t@relay.example?pin=p")
	if err != nil || def.Addr != "relay.example:8443" {
		t.Fatalf("default port: %+v, %v", def, err)
	}
	for _, bad := range []string{"https://t@h:1?pin=p", "satmux://h:1?pin=p", "satmux://t@h:1", ""} {
		if _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("ParseEndpoint(%q) accepted", bad)
		}
	}
}

func TestCopyHeaderDropsHopByHop(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "close, X-Private")
	src.Set("X-Private", "1")
	src.Set("Keep-Alive", "timeout=5")
	src.Set("Satmux-Token", "secret")
	src.Set("Authorization", "Bearer origin")
	src.Add("Set-Cookie", "a=1")
	src.Add("Set-Cookie", "b=2")
	dst := http.Header{}
	CopyHeader(dst, src)
	for _, k := range []string{"Connection", "X-Private", "Keep-Alive", "Satmux-Token"} {
		if dst.Get(k) != "" {
			t.Errorf("%s was copied", k)
		}
	}
	if dst.Get("Authorization") != "Bearer origin" || len(dst.Values("Set-Cookie")) != 2 {
		t.Errorf("end-to-end headers lost: %v", dst)
	}
}

func TestCompressible(t *testing.T) {
	random := make([]byte, 64<<10)
	_, _ = rand.Read(random)
	if Compressible(random) {
		t.Error("random bytes judged compressible")
	}
	if !Compressible(bytes.Repeat([]byte("<div class=x>hello</div>"), 1000)) {
		t.Error("HTML judged incompressible")
	}
}

// flushRecorder records how many bytes had been written at each flush.
type flushRecorder struct {
	bytes.Buffer
	flushes []int
}

func (f *flushRecorder) flush() error { f.flushes = append(f.flushes, f.Len()); return nil }

func TestPumpFlushesWhenTheSourceGoesQuiet(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("event 1\n"))
		time.Sleep(100 * time.Millisecond) // quiet period: must flush before this ends
		_, _ = pw.Write([]byte("event 2\n"))
		_ = pw.Close()
	}()
	var rec flushRecorder
	n, err := Pump(&rec, pr, rec.flush, 5*time.Millisecond)
	if err != nil || n != 16 {
		t.Fatalf("Pump = %d, %v", n, err)
	}
	if len(rec.flushes) < 2 || rec.flushes[0] != 8 {
		t.Fatalf("flushes at %v; want a flush after the first event, before the second arrived", rec.flushes)
	}
}

func TestBrotliCodecRoundTrip(t *testing.T) {
	src := bytes.Repeat([]byte(`{"k":"v","n":12345},`), 5000)
	var buf bytes.Buffer
	enc := NewEncoder(&buf, 11)
	_, _ = enc.Write(src[:1000])
	_ = enc.Flush() // a flushed prefix must be decodable before the stream ends
	if got, _ := io.ReadAll(io.LimitReader(NewDecoder(bytes.NewReader(buf.Bytes())), 1000)); !bytes.Equal(got, src[:1000]) {
		t.Fatal("flushed prefix not decodable")
	}
	_, _ = enc.Write(src[1000:])
	_ = enc.Close()
	got, err := io.ReadAll(NewDecoder(&buf))
	if err != nil || !bytes.Equal(got, src) {
		t.Fatalf("round trip failed: %v", err)
	}
	if buf.Len()*20 > len(src) {
		t.Errorf("compressed %d -> %d; expected >20x on repetitive JSON", len(src), buf.Len())
	}
}

// Levels shaped like a real calibration on a 2-vCPU cloud VM.
var vmLevels = []Level{
	{Quality: 4, Speed: 26e6, Ratio: 0.183},
	{Quality: 5, Speed: 22.8e6, Ratio: 0.171},
	{Quality: 9, Speed: 8e6, Ratio: 0.163},
	{Quality: 11, Speed: 0.3e6, Ratio: 0.118},
}

func TestChooseTradesCPUForBytes(t *testing.T) {
	cases := []struct {
		name     string
		linkRate float64
		busy     int
		want     int
	}{
		{"unknown link: cheap level", 0, 1, 5},
		{"fast link: CPU is the bottleneck", 5e6, 1, 4},
		{"seat-limited link: bytes are the bottleneck", 20e3, 1, 11},
		{"slow link but CPU contended", 20e3, 64, 9},
	}
	for _, c := range cases {
		if got := Choose(vmLevels, 200<<10, c.linkRate, c.busy).Quality; got != c.want {
			t.Errorf("%s: quality %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCalibrateMeasuresEveryLevel(t *testing.T) {
	if testing.Short() {
		t.Skip("calibration takes a second")
	}
	levels := Calibrate()
	if len(levels) != len(Qualities) {
		t.Fatalf("%d levels", len(levels))
	}
	for _, l := range levels {
		if l.Speed <= 0 || l.Ratio <= 0 || l.Ratio >= 1 {
			t.Errorf("implausible %+v", l)
		}
	}
	if levels[len(levels)-1].Ratio >= levels[0].Ratio {
		t.Errorf("highest quality did not compress better: %s", FormatLevels(levels))
	}
}
