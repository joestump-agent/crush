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

	"github.com/charmbracelet/crush/internal/filepathext"
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
	// presents. Relative paths resolve against the working directory.
	TLSCert string `json:"tls_cert,omitempty" jsonschema:"description=PEM certificate file for the A2A TCP listener. Relative paths resolve against the working directory,example=certs/a2a.pem"`
	// TLSKey is the PEM private key for TLSCert. Relative paths resolve
	// against the working directory.
	TLSKey string `json:"tls_key,omitempty" jsonschema:"description=PEM private key file for tls_cert. Relative paths resolve against the working directory,example=certs/a2a-key.pem"`
	// ClientCA, when set, turns on mutual TLS: every client must present
	// a certificate that chains to one of the PEM certificates in this
	// file, and a verified certificate authenticates the call.
	ClientCA string `json:"client_ca,omitempty" jsonschema:"description=PEM CA certificates. When set\\, the A2A TCP listener requires and verifies client certificates (mutual TLS),example=certs/clients-ca.pem"`
}

// Enabled reports whether a TCP listener is configured. The nil receiver
// and an empty Listen both mean no TCP listener.
func (a *A2AOptions) Enabled() bool {
	return a != nil && a.Listen != ""
}

// resolvePaths makes the TLS file paths absolute the way the data
// directory is: a leading ~ expands to the home directory, a relative
// path joins the working directory, and an absolute path is kept as is.
func (a *A2AOptions) resolvePaths(workingDir string) {
	if a == nil {
		return
	}
	for _, p := range []*string{&a.TLSCert, &a.TLSKey, &a.ClientCA} {
		if *p == "" {
			continue
		}
		*p = filepath.Clean(filepathext.SmartJoin(workingDir, home.Long(*p)))
	}
}

// Validate checks the TCP listener settings at load (#358): a listen
// address without both TLS files is refused, the address must be a
// host:port, the certificate and key must load as a pair, and a client
// CA file must hold at least one PEM certificate. The path names the
// block in the config file, so the errors point at the offending key.
func (a *A2AOptions) Validate(path string) error {
	if !a.Enabled() {
		return nil
	}
	if a.TLSCert == "" || a.TLSKey == "" {
		return errA2APlainTCP
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
// loads is one the host can serve. The path prefixes the errors.
func (a *A2AOptions) ServerTLSConfig(path string) (*tls.Config, error) {
	if a == nil || a.TLSCert == "" || a.TLSKey == "" {
		return nil, errA2APlainTCP
	}
	cert, err := tls.LoadX509KeyPair(a.TLSCert, a.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("%s.tls_cert, %s.tls_key: %w", path, path, err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if a.ClientCA != "" {
		pool, err := loadCertPool(a.ClientCA)
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
