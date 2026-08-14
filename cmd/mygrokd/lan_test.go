package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsRFC1918(t *testing.T) {
	yes := []string{
		"10.0.0.1",
		"10.255.255.255",
		"172.16.0.1",
		"172.31.255.255",
		"192.168.0.1",
		"192.168.255.255",
	}
	for _, ip := range yes {
		if !IsRFC1918(ip) {
			t.Errorf("%s should be RFC1918", ip)
		}
	}
	no := []string{
		"8.8.8.8",
		"172.15.0.1", // just outside 172.16/12
		"172.32.0.1", // just outside 172.16/12
		"192.167.0.1",
		"192.169.0.1",
		"9.255.255.255",
		"11.0.0.0",
		"::1",     // IPv6 not supported
		"fc00::1", // ULA — IPv6 private, we only do IPv4
		"not-an-ip",
		"",
	}
	for _, ip := range no {
		if IsRFC1918(ip) {
			t.Errorf("%s should NOT be RFC1918", ip)
		}
	}
}

func TestLANHostname(t *testing.T) {
	lm := newLANManager("Example.COM.", "", nil)
	got := lm.LANHostname("alice")
	want := "alice-lan.example.com"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestNewLANManagerNormalizesPublicHost(t *testing.T) {
	lm := newLANManager("EXAMPLE.com.", "/tmp", nil)
	if lm.publicHost != "example.com" {
		t.Errorf("expected lowercased, trimmed publicHost; got %q", lm.publicHost)
	}
	if lm.active == nil {
		t.Error("active map should be initialized")
	}
}

func TestWildcardCertPEMNilManager(t *testing.T) {
	var lm *lanManager
	_, _, err := lm.WildcardCertPEM()
	if err == nil {
		t.Error("expected error for nil manager")
	}
}

func TestWildcardCertPEMMissingFiles(t *testing.T) {
	lm := newLANManager("example.com", t.TempDir(), nil)
	_, _, err := lm.WildcardCertPEM()
	if err == nil {
		t.Error("expected error when no cert on disk")
	}
}

func TestWildcardCertPEMReadsMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	lm := newLANManager("example.com", dir, nil)

	// Mimic the certmagic on-disk layout: <dir>/certificates/<issuer>/wildcard_.example.com/wildcard_.example.com.{crt,key}
	certDir := filepath.Join(dir, "certificates", "acme-v02.api.letsencrypt.org-directory", "wildcard_.example.com")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	crtBody := "fake-cert-pem"
	keyBody := "fake-key-pem"
	if err := os.WriteFile(filepath.Join(certDir, "wildcard_.example.com.crt"), []byte(crtBody), 0o600); err != nil {
		t.Fatalf("write crt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "wildcard_.example.com.key"), []byte(keyBody), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	gotCert, gotKey, err := lm.WildcardCertPEM()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCert != crtBody {
		t.Errorf("cert: got %q want %q", gotCert, crtBody)
	}
	if gotKey != keyBody {
		t.Errorf("key: got %q want %q", gotKey, keyBody)
	}
}

func TestWildcardCertPEMMissingKeyFile(t *testing.T) {
	dir := t.TempDir()
	lm := newLANManager("example.com", dir, nil)
	certDir := filepath.Join(dir, "certificates", "acme", "wildcard_.example.com")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Only the .crt — the .key is missing.
	if err := os.WriteFile(filepath.Join(certDir, "wildcard_.example.com.crt"), []byte("c"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := lm.WildcardCertPEM(); err == nil {
		t.Error("expected error when key file missing")
	}
}
