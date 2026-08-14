package main

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"
)

// lanManager holds the bits needed to run the LAN-direct feature:
//
//   - read the wildcard cert/key off disk so we can ship them to
//     authenticated clients (they'll serve TLS for <sub>-lan.<publicHost>
//     directly on their LAN IP).
//   - write/delete public A records pointing the per-tunnel "<sub>-lan"
//     name at the client's RFC1918 LAN IP, through whichever libdns
//     provider --dns-provider selected.
//
// Disabled when --lan=false or when no cert is available yet. Best-effort:
// a DNS API outage doesn't take a tunnel down — the same-NAT 307 just
// won't fire until the next reconnect.
type lanManager struct {
	publicHost string
	certDir    string
	provider   dnsProvider

	mu     sync.Mutex
	active map[string]string // lan-hostname → IP we wrote
}

// newLANManager builds the manager. provider comes from --dns-provider and
// is never nil here: main refuses --lan without one, because a LAN-direct
// hostname is useless if we can't publish a record for it.
func newLANManager(publicHost, certDir string, provider dnsProvider) *lanManager {
	return &lanManager{
		publicHost: strings.ToLower(strings.TrimSuffix(publicHost, ".")),
		certDir:    certDir,
		provider:   provider,
		active:     map[string]string{},
	}
}

// LANHostname returns the per-tunnel sister hostname for a subdomain.
func (lm *lanManager) LANHostname(sub string) string {
	return sub + "-lan." + lm.publicHost
}

// WildcardCertPEM finds the wildcard cert + key on disk (managed by
// certmagic) and returns them as PEM strings. Returns ("", "", err) if
// the cert hasn't been issued yet — callers should treat that as "ship
// no cert this time" rather than fatal.
func (lm *lanManager) WildcardCertPEM() (cert, key string, err error) {
	if lm == nil {
		return "", "", fmt.Errorf("lan manager not configured")
	}
	// certmagic stores certs under:
	//   <cert-dir>/certificates/<issuer-dir>/<sanitized-name>/<sanitized-name>.{crt,key}
	// where "*.foo.com" is sanitized to "wildcard_.foo.com".
	sanitized := "wildcard_." + lm.publicHost
	pattern := filepath.Join(lm.certDir, "certificates", "*", sanitized, sanitized+".crt")
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return "", "", fmt.Errorf("wildcard cert not yet on disk at %s", pattern)
	}
	crtPath := matches[0]
	keyPath := strings.TrimSuffix(crtPath, ".crt") + ".key"
	crtBytes, err := os.ReadFile(crtPath)
	if err != nil {
		return "", "", fmt.Errorf("read cert: %w", err)
	}
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", fmt.Errorf("read key: %w", err)
	}
	return string(crtBytes), string(keyBytes), nil
}

// UpsertA writes a public A record for the per-tunnel sister hostname,
// pointing at the client's LAN IP. Idempotent — calling again with the
// same args is a cheap noop on Route53's side.
func (lm *lanManager) UpsertA(ctx context.Context, lanHostname, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse ip: %w", err)
	}
	if !addr.Is4() {
		return fmt.Errorf("only IPv4 supported for now (got %s)", ip)
	}
	relativeName := strings.TrimSuffix(lanHostname, "."+lm.publicHost)
	rec := libdns.Address{
		Name: relativeName,
		TTL:  60 * time.Second,
		IP:   addr,
	}
	if _, err := lm.provider.SetRecords(ctx, lm.publicHost+".", []libdns.Record{rec}); err != nil {
		return err
	}
	lm.mu.Lock()
	lm.active[lanHostname] = ip
	lm.mu.Unlock()
	return nil
}

// DeleteA removes the public A record for a per-tunnel sister hostname.
// Best-effort; an error is logged but doesn't bubble.
func (lm *lanManager) DeleteA(ctx context.Context, lanHostname string) error {
	lm.mu.Lock()
	ip, known := lm.active[lanHostname]
	delete(lm.active, lanHostname)
	lm.mu.Unlock()
	if !known {
		return nil
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return err
	}
	relativeName := strings.TrimSuffix(lanHostname, "."+lm.publicHost)
	rec := libdns.Address{
		Name: relativeName,
		TTL:  60 * time.Second,
		IP:   addr,
	}
	_, err = lm.provider.DeleteRecords(ctx, lm.publicHost+".", []libdns.Record{rec})
	return err
}

// IsRFC1918 reports whether ip is in a private IPv4 range. Used to
// validate the client's claimed LAN IP — we won't write a public DNS
// record pointing at, say, 8.8.8.8.
func IsRFC1918(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return false
	}
	if addr.IsPrivate() {
		return true
	}
	return false
}
