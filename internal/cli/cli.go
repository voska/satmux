// Package cli wires the satmux commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/voska/satmux/internal/client"
	"github.com/voska/satmux/internal/relay"
	"github.com/voska/satmux/internal/sysproxy"
	"github.com/voska/satmux/internal/wire"
)

// Exit codes.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitRetryable = 8  // the local client or the relay is not reachable
	ExitConfig    = 10 // no relay configured
)

// ExitError carries an exit code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func withCode(code int, err error) error { return &exitError{code, err} }

// Globals are flags shared by every command.
type Globals struct {
	JSON     bool   `help:"Print machine-readable JSON on stdout." env:"SATMUX_JSON"`
	LogLevel string `help:"Log level for stderr." default:"info" enum:"debug,info,warn,error" env:"SATMUX_LOG_LEVEL"`
	Version  string `kong:"-"`
}

// CLI is the command tree.
type CLI struct {
	Globals

	Up      UpCmd      `cmd:"" help:"Connect to your relay and serve the local proxy. Run on the laptop."`
	Join    JoinCmd    `cmd:"" help:"Save a relay connection string so 'satmux up' needs no flags."`
	Status  StatusCmd  `cmd:"" help:"Show carriers, traffic and compression of the running client."`
	Env     EnvCmd     `cmd:"" help:"Print shell exports that send command-line tools through satmux."`
	Bench   BenchCmd   `cmd:"" help:"Time a few URLs direct, through a tunnel, and through the gate."`
	Proxy   ProxyCmd   `cmd:"" help:"Turn the macOS system proxy on or off by hand."`
	Relay   RelayCmd   `cmd:"" help:"Run the relay or print its connection string. Run on the ground VM."`
	Version VersionCmd `cmd:"" help:"Print the version."`
}

// Run parses args and runs the selected command, returning the process exit code.
func Run(version string, args []string) int {
	var c CLI
	c.Globals.Version = version
	parser, err := kong.New(&c,
		kong.Name("satmux"),
		kong.Description("Make high-latency links (airplane Wi-Fi, GEO satellite) usable: fewer round trips, fewer bytes."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitError
	}
	kctx, err := parser.Parse(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "satmux:", err)
		return ExitUsage
	}
	level := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}[c.LogLevel]
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if err := kctx.Run(&c.Globals); err != nil {
		fmt.Fprintln(os.Stderr, "satmux:", err)
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		return ExitError
	}
	return ExitOK
}

func configDir() string {
	if d := os.Getenv("SATMUX_CONFIG_DIR"); d != "" {
		return d
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "satmux")
}

type clientConfig struct {
	Server string `json:"server"`
}

func loadServer(flag string) (wire.Endpoint, error) {
	s := flag
	if s == "" {
		b, err := os.ReadFile(filepath.Join(configDir(), "client.json"))
		if err == nil {
			var cfg clientConfig
			if json.Unmarshal(b, &cfg) == nil {
				s = cfg.Server
			}
		}
	}
	if s == "" {
		return wire.Endpoint{}, withCode(ExitConfig, errors.New("no relay configured: run `satmux join <connection string>` or pass --server"))
	}
	ep, err := wire.ParseEndpoint(s)
	if err != nil {
		return ep, withCode(ExitConfig, err)
	}
	return ep, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v) //nolint:wrapcheck // stdout
}

// ---------------------------------------------------------------- relay

// RelayCmd groups relay commands.
type RelayCmd struct {
	Run    RelayRunCmd    `cmd:"" default:"withargs" help:"Run the relay (default)."`
	Invite RelayInviteCmd `cmd:"" help:"Print the connection string clients need."`
}

type relayFlags struct {
	StateDir   string `help:"Where the relay keeps its key and token." env:"SATMUX_STATE_DIR"`
	PublicAddr string `help:"Host or IP clients dial (default: this machine's first public IPv4)." env:"SATMUX_PUBLIC_ADDR"`
}

func (f relayFlags) stateDir() string {
	if f.StateDir != "" {
		return f.StateDir
	}
	if os.Geteuid() == 0 {
		return "/var/lib/satmux"
	}
	return filepath.Join(configDir(), "relay")
}

func (f relayFlags) invite(st *relay.State, listen string) wire.Endpoint {
	_, port, _ := net.SplitHostPort(listen)
	p := wire.DefaultPort
	_, _ = fmt.Sscan(port, &p)
	host := f.PublicAddr
	if host == "" {
		host = relay.GuessPublicIP()
	}
	if host == "" {
		host = "YOUR_RELAY_IP"
	}
	return st.Invite(host, p)
}

// RelayRunCmd runs the relay.
type RelayRunCmd struct {
	relayFlags
	Listen  string `help:"Address to listen on. Avoid 443: cabin Wi-Fi often intercepts it." default:":8443" env:"SATMUX_LISTEN"`
	Quality int    `help:"Force a brotli quality (0-11) for responses. Default -1: calibrate this CPU and choose per response." default:"-1"`
}

// Run starts the relay.
func (r *RelayRunCmd) Run(g *Globals) error {
	st, err := relay.LoadState(r.stateDir())
	if err != nil {
		return fmt.Errorf("relay state: %w", err)
	}
	srv := relay.NewServer(r.Listen, st, relay.New(relay.Options{Token: st.Token, Quality: r.Quality, Log: slog.Default()}))
	ln, err := net.Listen("tcp", r.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("relay listening", "addr", ln.Addr().String(), "state", r.stateDir(), "version", g.Version)
	fmt.Fprintf(os.Stderr, "\nConnection string (keep it secret, it is the password):\n\n  %s\n\n", r.invite(st, r.Listen))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// RelayInviteCmd prints the connection string.
type RelayInviteCmd struct {
	relayFlags
	Listen string `help:"The relay's listen address (for the port)." default:":8443" env:"SATMUX_LISTEN"`
}

// Run prints the invite.
func (r *RelayInviteCmd) Run(g *Globals) error {
	st, err := relay.LoadState(r.stateDir())
	if err != nil {
		return fmt.Errorf("relay state: %w", err)
	}
	inv := r.invite(st, r.Listen)
	if g.JSON {
		return printJSON(map[string]string{"connection_string": inv.String(), "addr": inv.Addr, "pin": inv.Pin})
	}
	fmt.Println(inv)
	return nil
}

// ---------------------------------------------------------------- client

// JoinCmd saves the connection string.
type JoinCmd struct {
	ConnectionString string `arg:"" help:"The satmux://... string printed by the relay."`
}

// Run saves it.
func (j *JoinCmd) Run(g *Globals) error {
	ep, err := wire.ParseEndpoint(j.ConnectionString)
	if err != nil {
		return withCode(ExitUsage, err)
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, _ := json.Marshal(clientConfig{Server: ep.String()})
	path := filepath.Join(configDir(), "client.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if g.JSON {
		return printJSON(map[string]string{"saved": path, "relay": ep.Addr})
	}
	fmt.Fprintf(os.Stderr, "Saved relay %s to %s. Next: satmux up\n", ep.Addr, path)
	return nil
}

// UpCmd runs the client.
type UpCmd struct {
	Server      string `help:"Connection string (default: the one saved by 'satmux join')." env:"SATMUX_SERVER"`
	Carriers    int    `help:"Parallel connections to the relay." default:"4"`
	HTTP        string `help:"Local HTTP proxy and gate address." default:"127.0.0.1:8080" env:"SATMUX_HTTP"`
	SOCKS       string `help:"Local SOCKS5 address; empty disables it." default:"127.0.0.1:1080" env:"SATMUX_SOCKS"`
	Quality     int    `help:"Force a brotli quality (0-11). Default -1: measure the link and let the relay pick what its CPU can afford." default:"-1"`
	SystemProxy bool   `help:"Point the macOS system proxy at satmux while running, and restore it on exit." name:"system-proxy"`
}

// Run starts the client and blocks until interrupted.
func (u *UpCmd) Run(_ *Globals) error {
	ep, err := loadServer(u.Server)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	c := client.New(client.Options{Endpoint: ep, Carriers: u.Carriers, HTTPAddr: u.HTTP, SOCKSAddr: u.SOCKS, Quality: u.Quality, Log: slog.Default()})
	if err := c.Start(ctx); err != nil {
		return fmt.Errorf("start client: %w", err)
	}
	slog.Info("satmux up", "relay", ep.Addr, "http", c.HTTPAddr(), "socks", c.SOCKSAddr(), "carriers", u.Carriers)
	if u.SystemProxy {
		restore, err := enableSystemProxy(c.HTTPAddr(), c.SOCKSAddr(), ep)
		if err != nil {
			return err
		}
		defer restore()
	}
	<-ctx.Done()
	slog.Info("shutting down")
	return nil
}

func backupPath() string { return filepath.Join(configDir(), "sysproxy-backup.json") }

func enableSystemProxy(httpAddr, socksAddr string, ep wire.Endpoint) (func(), error) {
	svc, err := sysproxy.PrimaryService()
	if err != nil {
		return nil, err //nolint:wrapcheck // already descriptive
	}
	prev, err := sysproxy.Snapshot(svc)
	if err != nil {
		return nil, err //nolint:wrapcheck // already descriptive
	}
	// Keep an older backup if one exists: it describes the state before satmux ever ran.
	if _, err := os.Stat(backupPath()); errors.Is(err, os.ErrNotExist) {
		if err := sysproxy.SaveBackup(backupPath(), prev); err != nil {
			return nil, err //nolint:wrapcheck // already descriptive
		}
	} else if b, err := sysproxy.LoadBackup(backupPath()); err == nil {
		prev = b
	}
	host, _, _ := net.SplitHostPort(ep.Addr)
	if err := sysproxy.Enable(svc, httpAddr, socksAddr, append(append([]string{}, sysproxy.DefaultBypass...), host)); err != nil {
		return nil, err //nolint:wrapcheck // already descriptive
	}
	slog.Info("system proxy on", "service", svc)
	return func() {
		if err := sysproxy.Restore(prev); err != nil {
			slog.Error("could not restore the system proxy; run `satmux proxy off`", "err", err)
			return
		}
		_ = os.Remove(backupPath())
		slog.Info("system proxy restored", "service", svc)
	}, nil
}

// ProxyCmd toggles the macOS system proxy.
type ProxyCmd struct {
	On     ProxyOnCmd     `cmd:"" help:"Point the system proxy at a running 'satmux up'."`
	Off    ProxyOffCmd    `cmd:"" help:"Restore the system proxy as it was before satmux."`
	Status ProxyStatusCmd `cmd:"" help:"Show the current system proxy settings."`
}

// ProxyOnCmd enables the system proxy.
type ProxyOnCmd struct {
	HTTP  string `default:"127.0.0.1:8080" env:"SATMUX_HTTP" help:"satmux HTTP proxy address."`
	SOCKS string `default:"127.0.0.1:1080" env:"SATMUX_SOCKS" help:"satmux SOCKS5 address; empty to leave SOCKS alone."`
}

// Run enables it.
func (p *ProxyOnCmd) Run(_ *Globals) error {
	ep, _ := loadServer("")
	_, err := enableSystemProxy(p.HTTP, p.SOCKS, ep)
	return err
}

// ProxyOffCmd restores the system proxy.
type ProxyOffCmd struct{}

// Run restores it.
func (p *ProxyOffCmd) Run(_ *Globals) error {
	prev, err := sysproxy.LoadBackup(backupPath())
	if err != nil {
		svc, serr := sysproxy.PrimaryService()
		if serr != nil {
			return serr //nolint:wrapcheck // already descriptive
		}
		prev = sysproxy.Settings{Service: svc, Bypass: []string{"*.local", "169.254/16"}}
	}
	if err := sysproxy.Restore(prev); err != nil {
		return err //nolint:wrapcheck // already descriptive
	}
	_ = os.Remove(backupPath())
	fmt.Fprintf(os.Stderr, "System proxy restored on %s.\n", prev.Service)
	return nil
}

// ProxyStatusCmd prints the system proxy.
type ProxyStatusCmd struct{}

// Run prints it.
func (p *ProxyStatusCmd) Run(g *Globals) error {
	svc, err := sysproxy.PrimaryService()
	if err != nil {
		return err //nolint:wrapcheck // already descriptive
	}
	s, err := sysproxy.Snapshot(svc)
	if err != nil {
		return err //nolint:wrapcheck // already descriptive
	}
	if g.JSON {
		return printJSON(s)
	}
	for _, x := range []struct {
		name string
		p    sysproxy.Proxy
	}{{"http", s.Web}, {"https", s.Secure}, {"socks", s.SOCKS}} {
		state := "off"
		if x.p.Enabled {
			state = fmt.Sprintf("on  %s:%d", x.p.Host, x.p.Port)
		}
		fmt.Printf("%-6s %s\n", x.name, state)
	}
	fmt.Printf("service %s\nbypass  %s\n", s.Service, strings.Join(s.Bypass, ", "))
	return nil
}

// EnvCmd prints proxy exports.
type EnvCmd struct {
	HTTP string `default:"127.0.0.1:8080" env:"SATMUX_HTTP" help:"satmux HTTP proxy address."`
}

// Run prints exports for eval.
func (e *EnvCmd) Run(g *Globals) error {
	proxy := "http://" + e.HTTP
	noProxy := "localhost,127.0.0.1,::1,.local,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,.ts.net"
	if g.JSON {
		return printJSON(map[string]string{"HTTPS_PROXY": proxy, "HTTP_PROXY": proxy, "NO_PROXY": noProxy})
	}
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
		fmt.Printf("export %s=%s\n", k, proxy)
	}
	fmt.Printf("export NO_PROXY=%s\nexport no_proxy=%s\n", noProxy, noProxy)
	return nil
}

// StatusCmd shows the running client's state.
type StatusCmd struct {
	HTTP string `default:"127.0.0.1:8080" env:"SATMUX_HTTP" help:"satmux HTTP proxy address."`
}

func fetchStatus(addr string) (client.Status, error) {
	var s client.Status
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := hc.Get("http://" + addr + client.PathStatus)
	if err != nil {
		return s, withCode(ExitRetryable, fmt.Errorf("satmux is not running on %s (start it with `satmux up`): %w", addr, err))
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return s, fmt.Errorf("decode status: %w", err)
	}
	return s, nil
}

// Run prints status.
func (s *StatusCmd) Run(g *Globals) error {
	st, err := fetchStatus(s.HTTP)
	if err != nil {
		return err
	}
	if g.JSON {
		return printJSON(st)
	}
	var rtts []string
	for _, c := range st.Carriers {
		if c.Up {
			rtts = append(rtts, fmt.Sprintf("%.0fms", c.RTTms))
		} else {
			rtts = append(rtts, "down")
		}
	}
	fmt.Printf("relay     %s  (up %s)\n", st.Relay, time.Duration(st.UptimeSecs)*time.Second)
	fmt.Printf("carriers  %d/%d up  rtt %s\n", st.CarriersUp, len(st.Carriers), strings.Join(rtts, " "))
	fmt.Printf("down      %s delivered, %s over the link\n", human(st.BytesDown), human(st.WireDown))
	fmt.Printf("up        %s sent, %s over the link\n", human(st.BytesUp), human(st.WireUp))
	fmt.Printf("saved     %.0f%% of bytes\n", st.SavedPercent)
	fmt.Printf("requests  %d fetches, %d tunnels, %d errors\n", st.Fetches, st.Tunnels, st.Errors)
	return nil
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// VersionCmd prints the version.
type VersionCmd struct{}

// Run prints it.
func (v *VersionCmd) Run(g *Globals) error {
	if g.JSON {
		return printJSON(map[string]string{"version": g.Version})
	}
	fmt.Println(g.Version)
	return nil
}
