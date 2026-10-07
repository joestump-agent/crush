package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/charmbracelet/crush/internal/home"
)

// errA2APlainTCP is the load error for a TCP listener configured without
// TLS material (#358): the A2A host never serves plain TCP.
var errA2APlainTCP = errors.New("a2a.listen requires tls_cert and tls_key; plain TCP is not supported")

// A2AOptions configures the A2A host's optional TCP listener (#358). The
// host always serves its per-process unix socket; setting Listen adds a
// second, TLS-only listener on TCP that serves the same routes. Plain TCP
// is refused at load: Listen requires both TLSCert and TLSKey.
type A2AOptions struct {
	// Listen is the host:port the TCP listener binds. Empty (the
	// default) means no TCP listener at all.
	Listen string `json:"listen,omitempty" jsonschema:"description=host:port for the A2A host's TCP listener. Requires tls_cert and tls_key: plain TCP is refused,example=127.0.0.1:7443"`
	// TLSCert is the PEM server certificate (chain) the listener
	// presents. It must be absolute or start with ~/.
	TLSCert string `json:"tls_cert,omitempty" jsonschema:"description=PEM certificate file for the A2A TCP listener. Must be an absolute path or start with ~/,example=~/.config/crush/certs/a2a.pem"`
	// TLSKey is the PEM private key for TLSCert. It must be absolute or
	// start with ~/.
	TLSKey string `json:"tls_key,omitempty" jsonschema:"description=PEM private key file for tls_cert. Must be an absolute path or start with ~/,example=~/.config/crush/certs/a2a-key.pem"`
	// ClientCA, when set, turns on mutual TLS: every client must present
	// a certificate that chains to one of the PEM certificates in this
	// file, and a verified certificate authenticates the call. It must be
	// absolute or start with ~/.
	ClientCA string `json:"client_ca,omitempty" jsonschema:"description=PEM CA certificates. When set\\, the A2A TCP listener requires and verifies client certificates (mutual TLS). Must be an absolute path or start with ~/,example=~/.config/crush/certs/clients-ca.pem"`
}

// Enabled reports whether a TCP listener is configured. The nil receiver
// and an empty Listen both mean no TCP listener.
func (a *A2AOptions) Enabled() bool {
	return a != nil && a.Listen != ""
}

// expandPaths expands a leading ~ in the TLS file paths to the home
// directory, so the stored values are the files the host loads. Relative
// paths are left alone for Validate to refuse.
func (a *A2AOptions) expandPaths() {
	if a == nil {
		return
	}
	for _, p := range []*string{&a.TLSCert, &a.TLSKey, &a.ClientCA} {
		if *p != "" {
			*p = home.Long(*p)
		}
	}
}

// checkPaths refuses a relative TLS file path (#358). The listener is
// usually configured once, in the global config, while Crush runs in
// many projects: a relative path would resolve against each run's
// working directory, failing the load everywhere else and silently
// picking up a cloned repository's certs/ directory. An absolute path
// or one starting with ~/ means the same file wherever Crush runs.
func (a *A2AOptions) checkPaths(path string) error {
	for _, f := range []struct{ key, value string }{
		{"tls_cert", a.TLSCert},
		{"tls_key", a.TLSKey},
		{"client_ca", a.ClientCA},
	} {
		if f.value == "" || filepath.IsAbs(home.Long(f.value)) {
			continue
		}
		return fmt.Errorf("%s.%s: %q is a relative path; use an absolute path or one starting with ~/, since a relative path would resolve against whichever directory Crush runs in", path, f.key, f.value)
	}
	return nil
}

// Validate checks the TCP listener settings at load (#358): a listen
// address without both TLS files is refused, the TLS paths must be
// absolute or start with ~/, the address must be a host:port, the
// certificate and key must load as a pair, and a client CA file must
// hold at least one PEM certificate. The path names the block in the
// config file, so the errors point at the offending key.
func (a *A2AOptions) Validate(path string) error {
	if a == nil {
		return nil
	}
	if a.Enabled() && (a.TLSCert == "" || a.TLSKey == "") {
		return errA2APlainTCP
	}
	if err := a.checkPaths(path); err != nil {
		return err
	}
	if !a.Enabled() {
		return nil
	}
	_, port, err := net.SplitHostPort(a.Listen)
	if err != nil {
		return fmt.Errorf("%s.listen: %q is not a host:port address: %w", path, a.Listen, err)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || strconv.FormatUint(n, 10) != port {
		return fmt.Errorf("%s.listen: %q has an invalid port", path, a.Listen)
	}
	if _, err := a.ServerTLSConfig(path); err != nil {
		return err
	}
	return nil
}

// ServerTLSConfig loads the listener's TLS material into the server
// configuration the A2A host serves with (#358): TLS 1.2 or newer, the
// certificate pair, and — when ClientCA is set — required, verified
// client certificates. Validation and the host share it, so a config that
// loads is one the host can serve. Paths must be absolute or start with
// ~/. The path prefixes the errors.
func (a *A2AOptions) ServerTLSConfig(path string) (*tls.Config, error) {
	if a == nil || a.TLSCert == "" || a.TLSKey == "" {
		return nil, errA2APlainTCP
	}
	if err := a.checkPaths(path); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(home.Long(a.TLSCert), home.Long(a.TLSKey))
	if err != nil {
		return nil, fmt.Errorf("%s.tls_cert, %s.tls_key: %w", path, path, err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if a.ClientCA != "" {
		pool, err := loadCertPool(home.Long(a.ClientCA))
		if err != nil {
			return nil, fmt.Errorf("%s.client_ca: %w", path, err)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// loadCertPool reads a PEM file into a certificate pool. Every
// CERTIFICATE block must parse, any other block is an error, and the
// file must hold at least one certificate.
func loadCertPool(file string) (*x509.CertPool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	count := 0
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%s: unexpected PEM block %q, want CERTIFICATE", file, block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		pool.AddCert(cert)
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("%s: no PEM certificates found", file)
	}
	return pool, nil
}
