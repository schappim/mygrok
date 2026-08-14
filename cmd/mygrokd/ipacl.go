package main

// IP-level access control — per-tunnel allow / block lists, plus a
// global allow / block that applies to every tunnel.
//
// Default behaviour with empty lists is "open" — anyone can reach any
// tunnel. Adding a per-tunnel allowlist locks that tunnel down to the
// listed IPs. Adding a global blocklist hard-bans IPs across every
// tunnel. The check semantics:
//
//   1. Global blocklist hit → reject.
//   2. Per-tunnel blocklist hit → reject.
//   3. Per-tunnel allowlist non-empty AND no match → reject.
//   4. Per-tunnel allowlist empty AND global allowlist non-empty AND
//      no match → reject.
//   5. Otherwise → allow.
//
// State persists as JSON. The admin host (`tunnel.<host>`) is exempt
// (gating is in handlePublicConn) so you can't lock yourself out and
// `/install` stays reachable from any new machine.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

type ipEntry struct {
	Raw  string     `json:"raw"`
	Net  *net.IPNet `json:"-"`
	IP   net.IP     `json:"-"`
	Note string     `json:"note,omitempty"`
}

func (e ipEntry) contains(ip net.IP) bool {
	if e.Net != nil {
		return e.Net.Contains(ip)
	}
	if e.IP != nil {
		return e.IP.Equal(ip)
	}
	return false
}

type ipBucket struct {
	Allowed []ipEntry `json:"allowed,omitempty"`
	Blocked []ipEntry `json:"blocked,omitempty"`
}

func (b ipBucket) clone() ipBucket {
	return ipBucket{
		Allowed: append([]ipEntry(nil), b.Allowed...),
		Blocked: append([]ipEntry(nil), b.Blocked...),
	}
}

// ipACLFile is the on-disk shape. Tunnels is keyed by subdomain.
// The Legacy* fields are read on load (from pre-per-tunnel files) and
// promoted into Global; they are never written.
type ipACLFile struct {
	Global  ipBucket            `json:"global,omitempty"`
	Tunnels map[string]ipBucket `json:"tunnels,omitempty"`

	LegacyAllowed []ipEntry `json:"allowed,omitempty"`
	LegacyBlocked []ipEntry `json:"blocked,omitempty"`
}

type ipACL struct {
	mu      sync.RWMutex
	path    string
	global  ipBucket
	tunnels map[string]ipBucket
}

func newIPACL(path string) (*ipACL, error) {
	a := &ipACL{path: path, tunnels: map[string]ipBucket{}}
	if err := a.load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return a, nil
}

func (a *ipACL) load() error {
	b, err := os.ReadFile(a.path)
	if err != nil {
		return err
	}
	var f ipACLFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse %s: %w", a.path, err)
	}

	global, err := parseBucket(f.Global)
	if err != nil {
		return err
	}
	// Promote legacy flat allowed/blocked into Global (one-way migration).
	if len(f.LegacyAllowed) > 0 || len(f.LegacyBlocked) > 0 {
		legacyAllowed, err := parseEntries(f.LegacyAllowed)
		if err != nil {
			return err
		}
		legacyBlocked, err := parseEntries(f.LegacyBlocked)
		if err != nil {
			return err
		}
		global.Allowed = append(global.Allowed, legacyAllowed...)
		global.Blocked = append(global.Blocked, legacyBlocked...)
	}
	tunnels := map[string]ipBucket{}
	for sub, b := range f.Tunnels {
		parsed, err := parseBucket(b)
		if err != nil {
			return fmt.Errorf("tunnel %q: %w", sub, err)
		}
		tunnels[sub] = parsed
	}

	a.mu.Lock()
	a.global = global
	a.tunnels = tunnels
	saveAfterMigration := len(f.LegacyAllowed) > 0 || len(f.LegacyBlocked) > 0
	a.mu.Unlock()
	if saveAfterMigration {
		// Re-write so the legacy fields disappear from disk.
		a.mu.Lock()
		_ = a.saveLocked()
		a.mu.Unlock()
	}
	return nil
}

func parseBucket(in ipBucket) (ipBucket, error) {
	out := ipBucket{}
	allowed, err := parseEntries(in.Allowed)
	if err != nil {
		return out, err
	}
	blocked, err := parseEntries(in.Blocked)
	if err != nil {
		return out, err
	}
	out.Allowed = allowed
	out.Blocked = blocked
	return out, nil
}

func parseEntries(in []ipEntry) ([]ipEntry, error) {
	out := make([]ipEntry, 0, len(in))
	for _, e := range in {
		parsed, err := parseIPOrCIDR(e.Raw)
		if err != nil {
			return nil, fmt.Errorf("invalid stored entry %q: %w", e.Raw, err)
		}
		parsed.Note = e.Note
		out = append(out, parsed)
	}
	return out, nil
}

func parseIPOrCIDR(s string) (ipEntry, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ipEntry{}, fmt.Errorf("empty")
	}
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return ipEntry{}, fmt.Errorf("invalid CIDR %q", s)
		}
		return ipEntry{Raw: n.String(), Net: n}, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return ipEntry{}, fmt.Errorf("invalid IP %q", s)
	}
	return ipEntry{Raw: ip.String(), IP: ip}, nil
}

func (a *ipACL) saveLocked() error {
	if a.path == "" {
		return nil
	}
	if err := os.MkdirAll(dirOf(a.path), 0o755); err != nil {
		return err
	}
	out := ipACLFile{Global: a.global}
	if len(a.tunnels) > 0 {
		// Drop empty tunnel entries so the file stays tidy after the
		// last rule for a tunnel is removed.
		out.Tunnels = map[string]ipBucket{}
		for sub, b := range a.tunnels {
			if len(b.Allowed) == 0 && len(b.Blocked) == 0 {
				continue
			}
			out.Tunnels[sub] = b
		}
		if len(out.Tunnels) == 0 {
			out.Tunnels = nil
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

// Check returns true if ip is permitted to reach the given tunnel.
// Pass subdomain="" when no tunnel is identified (e.g., custom-host
// lookups that miss); the per-tunnel rules are skipped in that case
// and only the global lists apply.
func (a *ipACL) Check(subdomain string, ip net.IP) bool {
	if ip == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Global blocklist always wins.
	for _, e := range a.global.Blocked {
		if e.contains(ip) {
			return false
		}
	}

	if subdomain != "" {
		if t, ok := a.tunnels[subdomain]; ok {
			for _, e := range t.Blocked {
				if e.contains(ip) {
					return false
				}
			}
			if len(t.Allowed) > 0 {
				for _, e := range t.Allowed {
					if e.contains(ip) {
						return true
					}
				}
				return false
			}
		}
	}

	if len(a.global.Allowed) == 0 {
		return true
	}
	for _, e := range a.global.Allowed {
		if e.contains(ip) {
			return true
		}
	}
	return false
}

// SnapshotGlobal returns a copy of the global bucket for display.
func (a *ipACL) SnapshotGlobal() ipBucket {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.global.clone()
}

// SnapshotTunnel returns a copy of one tunnel's bucket.
func (a *ipACL) SnapshotTunnel(sub string) ipBucket {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tunnels[sub].clone()
}

// TunnelsConfigured returns subdomains that have any rules configured,
// sorted lexically.
func (a *ipACL) TunnelsConfigured() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []string
	for sub, b := range a.tunnels {
		if len(b.Allowed) > 0 || len(b.Blocked) > 0 {
			out = append(out, sub)
		}
	}
	sort.Strings(out)
	return out
}

// Add adds raw to the chosen list. scope is "global" for the global
// lists or a subdomain string for per-tunnel rules.
func (a *ipACL) Add(scope, list, raw, note string) error {
	entry, err := parseIPOrCIDR(raw)
	if err != nil {
		return err
	}
	entry.Note = strings.TrimSpace(note)
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.bucketLocked(scope, true)
	if bucket == nil {
		return fmt.Errorf("invalid scope %q", scope)
	}
	target := bucketSlice(bucket, list)
	if target == nil {
		return fmt.Errorf("unknown list %q (use allow|block)", list)
	}
	for _, e := range *target {
		if e.Raw == entry.Raw {
			return fmt.Errorf("%s already in %s/%s list", entry.Raw, scope, list)
		}
	}
	*target = append(*target, entry)
	a.writeBucketLocked(scope, *bucket)
	return a.saveLocked()
}

// Remove drops raw from the list.
func (a *ipACL) Remove(scope, list, raw string) error {
	entry, err := parseIPOrCIDR(raw)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.bucketLocked(scope, false)
	if bucket == nil {
		return fmt.Errorf("nothing configured for %q", scope)
	}
	target := bucketSlice(bucket, list)
	if target == nil {
		return fmt.Errorf("unknown list %q", list)
	}
	out := (*target)[:0]
	found := false
	for _, e := range *target {
		if e.Raw == entry.Raw {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return fmt.Errorf("%s not in %s/%s list", entry.Raw, scope, list)
	}
	*target = out
	a.writeBucketLocked(scope, *bucket)
	return a.saveLocked()
}

// bucketLocked returns a pointer to the bucket for scope. With
// createMissing=true, a per-tunnel scope's bucket is materialised on
// first use; otherwise nil is returned for unknown scopes.
func (a *ipACL) bucketLocked(scope string, createMissing bool) *ipBucket {
	if scope == "global" || scope == "" {
		return &a.global
	}
	if !looksLikeSubdomain(scope) {
		return nil
	}
	b, ok := a.tunnels[scope]
	if !ok {
		if !createMissing {
			return nil
		}
		b = ipBucket{}
		a.tunnels[scope] = b
	}
	// Return a pointer that survives the map-value semantics by
	// returning the addressable map slot via re-assignment after
	// the caller mutates. We do that via writeBucketLocked.
	out := b
	return &out
}

// writeBucketLocked writes back the modified bucket. Necessary because
// Go maps return copies, not references, for value types.
func (a *ipACL) writeBucketLocked(scope string, b ipBucket) {
	if scope == "global" || scope == "" {
		a.global = b
		return
	}
	if len(b.Allowed) == 0 && len(b.Blocked) == 0 {
		delete(a.tunnels, scope)
		return
	}
	a.tunnels[scope] = b
}

func bucketSlice(b *ipBucket, list string) *[]ipEntry {
	switch list {
	case "allow", "allowed", "allowlist":
		return &b.Allowed
	case "block", "blocked", "blocklist", "deny":
		return &b.Blocked
	default:
		return nil
	}
}

// looksLikeSubdomain matches the same shape validSubdomain enforces in
// main.go; duplicated here so ipacl.go has no cross-file dependency on
// the routing rules. (Subdomain validation is intentionally strict —
// admin scope strings cannot wedge in arbitrary characters.)
func looksLikeSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return true
}
