package service

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/testutil/mtls"
)

func TestBuildRelayIngressTLSConfig_RequiresClientCert(t *testing.T) {
	ca := mtls.NewCA(t)
	srvCert, srvKey := ca.LeafPEM(t, "relay-ingress", true, "127.0.0.1")
	dir := t.TempDir()
	writeRelayPEM(t, dir, "s.crt", srvCert)
	writeRelayPEM(t, dir, "s.key", srvKey)
	writeRelayPEM(t, dir, "ca.crt", ca.CertPEM)

	cfg := config.RelayIngressConfig{
		Addr:       ":0",
		ServerCert: dir + "/s.crt",
		ServerKey:  dir + "/s.key",
		ClientCA:   dir + "/ca.crt",
	}
	tlsCfg, err := buildRelayIngressTLSConfig(cfg)
	if err != nil {
		t.Fatalf("build tls config: %v", err)
	}
	if tlsCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("relay ingress must require+verify client certs, got %v", tlsCfg.ClientAuth)
	}
	if tlsCfg.ClientCAs == nil {
		t.Fatal("relay ingress must set ClientCAs")
	}
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("relay ingress must load exactly one server cert, got %d", len(tlsCfg.Certificates))
	}
}

// TestBuildRelayIngressTLSConfig_MissingServerKeyPair asserts that a
// non-existent ServerCert or ServerKey path returns an error rather
// than silently producing a zero-value TLS config.
func TestBuildRelayIngressTLSConfig_MissingServerKeyPair(t *testing.T) {
	ca := mtls.NewCA(t)
	dir := t.TempDir()
	writeRelayPEM(t, dir, "ca.crt", ca.CertPEM)

	cfg := config.RelayIngressConfig{
		Addr:       ":0",
		ServerCert: dir + "/nonexistent.crt",
		ServerKey:  dir + "/nonexistent.key",
		ClientCA:   dir + "/ca.crt",
	}
	_, err := buildRelayIngressTLSConfig(cfg)
	if err == nil {
		t.Fatal("expected error for missing server cert/key, got nil")
	}
}

// TestBuildRelayIngressTLSConfig_InvalidClientCA asserts that a ClientCA
// file that contains no valid PEM cert block returns an error rather than
// accepting an empty trust pool.
func TestBuildRelayIngressTLSConfig_InvalidClientCA(t *testing.T) {
	ca := mtls.NewCA(t)
	srvCert, srvKey := ca.LeafPEM(t, "relay-ingress", true, "127.0.0.1")
	dir := t.TempDir()
	writeRelayPEM(t, dir, "s.crt", srvCert)
	writeRelayPEM(t, dir, "s.key", srvKey)
	// Write junk bytes — no valid PEM CERTIFICATE block.
	junkPath := filepath.Join(dir, "junk-ca.crt")
	if err := os.WriteFile(junkPath, []byte("this is not a pem cert\n"), 0o600); err != nil {
		t.Fatalf("write junk ca: %v", err)
	}

	cfg := config.RelayIngressConfig{
		Addr:       ":0",
		ServerCert: dir + "/s.crt",
		ServerKey:  dir + "/s.key",
		ClientCA:   junkPath,
	}
	_, err := buildRelayIngressTLSConfig(cfg)
	if err == nil {
		t.Fatal("expected error for ClientCA with no valid PEM cert block, got nil")
	}
}

// writeRelayPEM writes pem bytes to filepath.Join(dir, name) with mode 0o600.
func writeRelayPEM(t *testing.T, dir, name string, pemBytes []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), pemBytes, 0o600); err != nil {
		t.Fatalf("writeRelayPEM %s: %v", name, err)
	}
}
