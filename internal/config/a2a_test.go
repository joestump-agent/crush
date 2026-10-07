package config_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/require"
)

// plainTCPRefused is the exact load error for a listen address without
// TLS material (#358).
const plainTCPRefused = "a2a.listen requires tls_cert and tls_key; plain TCP is not supported"

// a2aTestCert writes a self-signed certificate and its key as PEM files
// under dir/certs and returns their paths relative to dir. The
// certificate doubles as a client CA bundle.
func a2aTestCert(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "certs"), 0o755))
	cert = filepath.Join("certs", "a2a.pem")
	key = filepath.Join("certs", "a2a-key.pem")
	require.NoError(t, os.WriteFile(filepath.Join(dir, cert), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, key), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	return cert, key
}

func TestA2AOptionsFromJSON(t *testing.T) {
	t.Parallel()

	var cfg config.Config
	require.NoError(t, json.Unmarshal([]byte(`{"options":{"a2a":{
		"listen": "127.0.0.1:7443",
		"tls_cert": "certs/a2a.pem",
		"tls_key": "certs/a2a-key.pem",
		"client_ca": "certs/ca.pem"
	}}}`), &cfg))
	require.Equal(t, &config.A2AOptions{
		Listen:   "127.0.0.1:7443",
		TLSCert:  "certs/a2a.pem",
		TLSKey:   "certs/a2a-key.pem",
		ClientCA: "certs/ca.pem",
	}, cfg.Options.A2A)
	require.True(t, cfg.Options.A2A.Enabled())
}

func TestA2AOptionsValidate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certRel, keyRel := a2aTestCert(t, dir)
	cert, key := filepath.Join(dir, certRel), filepath.Join(dir, keyRel)
	otherDir := t.TempDir()
	otherCertRel, otherKeyRel := a2aTestCert(t, otherDir)
	otherKey := filepath.Join(otherDir, otherKeyRel)
	notPEM := filepath.Join(dir, "not.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))

	tests := []struct {
		name    string
		opts    *config.A2AOptions
		wantErr string
	}{
		{name: "nil block", opts: nil},
		{name: "empty block", opts: &config.A2AOptions{}},
		{
			name: "files without listen start nothing",
			opts: &config.A2AOptions{TLSCert: cert},
		},
		{
			name: "valid listener",
			opts: &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert, TLSKey: key},
		},
		{
			name: "valid listener with mutual TLS",
			opts: &config.A2AOptions{Listen: ":0", TLSCert: cert, TLSKey: key, ClientCA: filepath.Join(otherDir, otherCertRel)},
		},
		{
			name:    "plain TCP",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443"},
			wantErr: plainTCPRefused,
		},
		{
			name:    "certificate without key",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert},
			wantErr: plainTCPRefused,
		},
		{
			name:    "key without certificate",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSKey: key},
			wantErr: plainTCPRefused,
		},
		{
			name:    "address without port",
			opts:    &config.A2AOptions{Listen: "127.0.0.1", TLSCert: cert, TLSKey: key},
			wantErr: "options.a2a.listen",
		},
		{
			name:    "port out of range",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:70000", TLSCert: cert, TLSKey: key},
			wantErr: "options.a2a.listen",
		},
		{
			name:    "missing certificate file",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: filepath.Join(dir, "nope.pem"), TLSKey: key},
			wantErr: "options.a2a.tls_cert",
		},
		{
			name:    "mismatched key",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert, TLSKey: otherKey},
			wantErr: "options.a2a.tls_cert",
		},
		{
			name:    "client CA is not PEM",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert, TLSKey: key, ClientCA: notPEM},
			wantErr: "options.a2a.client_ca",
		},
		{
			name:    "client CA holds a key, not certificates",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert, TLSKey: key, ClientCA: key},
			wantErr: "options.a2a.client_ca",
		},
		{
			name:    "missing client CA",
			opts:    &config.A2AOptions{Listen: "127.0.0.1:7443", TLSCert: cert, TLSKey: key, ClientCA: filepath.Join(dir, "nope.pem")},
			wantErr: "options.a2a.client_ca",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.opts.Validate("options.a2a")
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tt.wantErr == plainTCPRefused {
				require.EqualError(t, err, plainTCPRefused)
				return
			}
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// The listener's TLS configuration (#358): TLS 1.2 at least, and client
// certificates required and verified only when client_ca is set.
func TestA2AOptionsServerTLSConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certRel, keyRel := a2aTestCert(t, dir)
	opts := &config.A2AOptions{
		Listen:  "127.0.0.1:0",
		TLSCert: filepath.Join(dir, certRel),
		TLSKey:  filepath.Join(dir, keyRel),
	}

	cfg, err := opts.ServerTLSConfig("options.a2a")
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	require.Len(t, cfg.Certificates, 1)
	require.Equal(t, tls.NoClientCert, cfg.ClientAuth)
	require.Nil(t, cfg.ClientCAs)

	opts.ClientCA = opts.TLSCert
	cfg, err = opts.ServerTLSConfig("options.a2a")
	require.NoError(t, err)
	require.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.NotNil(t, cfg.ClientCAs)

	_, err = (&config.A2AOptions{Listen: "127.0.0.1:0"}).ServerTLSConfig("options.a2a")
	require.EqualError(t, err, plainTCPRefused)
}

// isolateA2AConfig points the global config and data paths at a fresh
// directory, so only the file under test contributes. No t.Parallel():
// it sets env vars.
func isolateA2AConfig(t *testing.T) {
	t.Helper()
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(isolated, ".config", "crush"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(isolated, ".local", "share", "crush"))
}

// loadA2AConfig writes one config file into workDir and loads it.
func loadA2AConfig(t *testing.T, workDir, name, content string) (*config.ConfigStore, error) {
	t.Helper()
	path := filepath.Join(workDir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	t.Cleanup(func() { _ = os.Remove(path) })
	return config.Load(workDir, t.TempDir(), false)
}

// The crushrc options produce the same options.a2a as the JSON keys
// (#358), down to the TLS paths, which both resolve against the working
// directory.
func TestA2ACrushrcMatchesJSON(t *testing.T) {
	isolateA2AConfig(t)
	workDir := t.TempDir()
	cert, key := a2aTestCert(t, workDir)

	rc, err := loadA2AConfig(t, workDir, "crushrc", `option a2a-listen 127.0.0.1:7443
option a2a-tls-cert `+filepath.ToSlash(cert)+`
option a2a-tls-key `+filepath.ToSlash(key)+`
option a2a-client-ca `+filepath.ToSlash(cert))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(workDir, "crushrc")))

	body, err := json.Marshal(map[string]any{"options": map[string]any{"a2a": map[string]any{
		"listen":    "127.0.0.1:7443",
		"tls_cert":  filepath.ToSlash(cert),
		"tls_key":   filepath.ToSlash(key),
		"client_ca": filepath.ToSlash(cert),
	}}})
	require.NoError(t, err)
	js, err := loadA2AConfig(t, workDir, "crush.json", string(body))
	require.NoError(t, err)

	want := &config.A2AOptions{
		Listen:   "127.0.0.1:7443",
		TLSCert:  filepath.Join(workDir, cert),
		TLSKey:   filepath.Join(workDir, key),
		ClientCA: filepath.Join(workDir, cert),
	}
	require.Equal(t, want, js.Config().Options.A2A, "relative paths resolve against the working directory")
	require.Equal(t, js.Config().Options.A2A, rc.Config().Options.A2A, "crushrc and crush.json load the same block")
}

// A listen address without TLS material fails the load (#358), from
// either config format, with the same message.
func TestA2APlainTCPRefusedAtLoad(t *testing.T) {
	isolateA2AConfig(t)

	_, err := loadA2AConfig(t, t.TempDir(), "crushrc", `option a2a-listen 127.0.0.1:7443`)
	require.Error(t, err)
	require.Contains(t, err.Error(), plainTCPRefused)

	_, err = loadA2AConfig(t, t.TempDir(), "crush.json", `{"options":{"a2a":{"listen":"127.0.0.1:7443","tls_cert":"a2a.pem"}}}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), plainTCPRefused)
}

// With no a2a block nothing is enabled (#358).
func TestA2ANoConfigLoadsNoListener(t *testing.T) {
	isolateA2AConfig(t)

	store, err := loadA2AConfig(t, t.TempDir(), "crushrc", `option debug true`)
	require.NoError(t, err)
	require.False(t, store.Config().Options.A2A.Enabled())
}
