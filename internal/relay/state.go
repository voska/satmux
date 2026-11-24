package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/voska/satmux/internal/wire"
)

// State is the relay's identity: a long-lived key pair and the shared token.
// It lives in a directory with mode 0700 and is created on first run.
type State struct {
	Cert  tls.Certificate
	Pin   string
	Token string
}

// LoadState reads the relay identity from dir, creating it if absent.
func LoadState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	certPath, keyPath, tokenPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "token")
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		if err := generate(certPath, keyPath, tokenPath); err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load relay key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse relay certificate: %w", err)
	}
	token, err := os.ReadFile(tokenPath) // #nosec G304 -- path is inside the operator's state dir
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	return &State{Cert: cert, Pin: wire.Pin(leaf), Token: strings.TrimSpace(string(token))}, nil
}

func generate(certPath, keyPath, tokenPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "satmux relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"satmux"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	files := []struct {
		path string
		data []byte
	}{
		{keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})},
		{certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})},
		{tokenPath, []byte(base64.RawURLEncoding.EncodeToString(tok) + "\n")},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}
	return nil
}

// Invite builds the connection string clients use, for the given public address.
func (s *State) Invite(publicHost string, port int) wire.Endpoint {
	return wire.Endpoint{Addr: net.JoinHostPort(publicHost, strconv.Itoa(port)), Token: s.Token, Pin: s.Pin}
}

// GuessPublicIP returns the first global unicast IPv4 address on this machine's
// interfaces, which is the public address on most VPS providers.
func GuessPublicIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !inCGNAT(ip) {
			return ip.String()
		}
	}
	return ""
}

func inCGNAT(ip net.IP) bool { return ip[0] == 100 && ip[1]&0xc0 == 64 }
