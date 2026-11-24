// Package sysproxy points the macOS system proxy at satmux and puts it back afterwards.
package sysproxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ErrUnsupported is returned on systems other than macOS.
var ErrUnsupported = errors.New("system proxy control is only implemented for macOS; export HTTPS_PROXY instead (see `satmux env`)")

// DefaultBypass keeps local, private and tailnet traffic off the relay, so tools
// like Tailscale and SSH never depend on it.
var DefaultBypass = []string{
	"localhost", "127.0.0.1", "::1", "*.local", "169.254/16",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
	"*.ts.net", "*.tailscale.com", "*.tailscale.io",
}

// Proxy is one proxy setting of a network service.
type Proxy struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
}

// Settings is the proxy configuration of one network service.
type Settings struct {
	Service string   `json:"service"`
	Web     Proxy    `json:"web"`
	Secure  Proxy    `json:"secure"`
	SOCKS   Proxy    `json:"socks"`
	Bypass  []string `json:"bypass"`
}

func networksetup(args ...string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrUnsupported
	}
	out, err := exec.Command("networksetup", args...).CombinedOutput() // #nosec G204 -- fixed binary, argv only
	if err != nil {
		return "", fmt.Errorf("networksetup %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

// PrimaryService returns the network service (e.g. "Wi-Fi") that carries the default route.
func PrimaryService() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", ErrUnsupported
	}
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err != nil {
		return "", fmt.Errorf("find default route: %w", err)
	}
	var iface string
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "interface" {
			iface = strings.TrimSpace(v)
		}
	}
	ports, err := networksetup("-listallhardwareports")
	if err != nil {
		return "", err
	}
	var port string
	sc := bufio.NewScanner(strings.NewReader(ports))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
			port = v
		}
		if v, ok := strings.CutPrefix(line, "Device: "); ok && v == iface && iface != "" {
			return port, nil
		}
	}
	return "Wi-Fi", nil
}

func getProxy(service, kind string) (Proxy, error) {
	out, err := networksetup("-get"+kind, service)
	if err != nil {
		return Proxy{}, err
	}
	var p Proxy
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Enabled":
			p.Enabled = v == "Yes"
		case "Server":
			p.Host = v
		case "Port":
			p.Port, _ = strconv.Atoi(v)
		}
	}
	return p, nil
}

// Snapshot reads the current proxy settings of a service.
func Snapshot(service string) (Settings, error) {
	s := Settings{Service: service}
	var err error
	if s.Web, err = getProxy(service, "webproxy"); err != nil {
		return s, err
	}
	if s.Secure, err = getProxy(service, "securewebproxy"); err != nil {
		return s, err
	}
	if s.SOCKS, err = getProxy(service, "socksfirewallproxy"); err != nil {
		return s, err
	}
	out, err := networksetup("-getproxybypassdomains", service)
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" && !strings.Contains(line, "There aren't any") {
			s.Bypass = append(s.Bypass, strings.TrimSpace(line))
		}
	}
	return s, nil
}

func setProxy(service, kind string, p Proxy) error {
	if p.Host != "" {
		if _, err := networksetup("-set"+kind, service, p.Host, strconv.Itoa(p.Port)); err != nil {
			return err
		}
	}
	state := "off"
	if p.Enabled {
		state = "on"
	}
	_, err := networksetup("-set"+kind+"state", service, state)
	return err
}

// Restore applies a snapshot.
func Restore(s Settings) error {
	bypass := s.Bypass
	if len(bypass) == 0 {
		bypass = []string{"Empty"}
	}
	if _, err := networksetup(append([]string{"-setproxybypassdomains", s.Service}, bypass...)...); err != nil {
		return err
	}
	for _, x := range []struct {
		kind string
		p    Proxy
	}{{"webproxy", s.Web}, {"securewebproxy", s.Secure}, {"socksfirewallproxy", s.SOCKS}} {
		if err := setProxy(s.Service, x.kind, x.p); err != nil {
			return err
		}
	}
	return nil
}

// Enable points a service at satmux's HTTP proxy (and SOCKS5, if socksAddr is set).
func Enable(service, httpAddr, socksAddr string, bypass []string) error {
	hh, hp, err := splitAddr(httpAddr)
	if err != nil {
		return err
	}
	s := Settings{
		Service: service, Bypass: bypass,
		Web:    Proxy{Enabled: true, Host: hh, Port: hp},
		Secure: Proxy{Enabled: true, Host: hh, Port: hp},
	}
	if socksAddr != "" {
		sh, sp, err := splitAddr(socksAddr)
		if err != nil {
			return err
		}
		s.SOCKS = Proxy{Enabled: true, Host: sh, Port: sp}
	}
	return Restore(s)
}

func splitAddr(addr string) (string, int, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("bad address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("bad port in %q: %w", addr, err)
	}
	return h, port, nil
}

// SaveBackup stores the pre-satmux settings so `satmux proxy off` can restore them
// even if the client crashed.
func SaveBackup(path string, s Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write proxy backup: %w", err)
	}
	return nil
}

// LoadBackup reads a backup written by SaveBackup.
func LoadBackup(path string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path) // #nosec G304 -- our own config path
	if err != nil {
		return s, fmt.Errorf("read proxy backup: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse proxy backup: %w", err)
	}
	return s, nil
}
