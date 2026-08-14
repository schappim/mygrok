package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestParseIPOrCIDR(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		e, err := parseIPOrCIDR("203.0.113.1")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if e.IP == nil || e.Net != nil {
			t.Errorf("expected IP-only entry, got %+v", e)
		}
		if e.Raw != "203.0.113.1" {
			t.Errorf("raw: %q", e.Raw)
		}
	})
	t.Run("cidr", func(t *testing.T) {
		e, err := parseIPOrCIDR("10.0.0.0/8")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if e.Net == nil || e.IP != nil {
			t.Errorf("expected Net-only entry, got %+v", e)
		}
		if e.Raw != "10.0.0.0/8" {
			t.Errorf("raw: %q", e.Raw)
		}
	})
	t.Run("trimmed", func(t *testing.T) {
		e, err := parseIPOrCIDR("   1.2.3.4   ")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if e.Raw != "1.2.3.4" {
			t.Errorf("expected trimmed raw, got %q", e.Raw)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		for _, in := range []string{"", "   ", "999.999.999.999", "not-ip", "10.0.0.0/40"} {
			if _, err := parseIPOrCIDR(in); err == nil {
				t.Errorf("expected error for %q", in)
			}
		}
	})
}

func TestLooksLikeSubdomain(t *testing.T) {
	for _, s := range []string{"a", "abc", "abc-def", "a1b2"} {
		if !looksLikeSubdomain(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "-a", "a-", "UPPER", "has.dot", "under_score"} {
		if looksLikeSubdomain(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestIPEntryContains(t *testing.T) {
	cidr, _ := parseIPOrCIDR("10.0.0.0/8")
	if !cidr.contains(net.ParseIP("10.1.2.3")) {
		t.Error("CIDR should contain 10.1.2.3")
	}
	if cidr.contains(net.ParseIP("11.0.0.1")) {
		t.Error("CIDR should not contain 11.0.0.1")
	}

	ip, _ := parseIPOrCIDR("1.2.3.4")
	if !ip.contains(net.ParseIP("1.2.3.4")) {
		t.Error("exact IP should match itself")
	}
	if ip.contains(net.ParseIP("1.2.3.5")) {
		t.Error("exact IP should not match neighbour")
	}

	// Zero-value entry contains nothing.
	if (ipEntry{}.contains(net.ParseIP("1.2.3.4"))) {
		t.Error("zero entry should contain nothing")
	}
}

func TestACLDefaultOpen(t *testing.T) {
	acl := mustACL(t)
	if !acl.Check("alice", net.ParseIP("203.0.113.1")) {
		t.Error("empty ACL should allow")
	}
	if !acl.Check("", net.ParseIP("203.0.113.1")) {
		t.Error("empty ACL should allow even without subdomain")
	}
	if !acl.Check("alice", nil) {
		t.Error("nil IP is allowed (we couldn't classify it anyway)")
	}
}

func TestACLGlobalBlocklistBlocksEveryone(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("global", "block", "203.0.113.0/24", "naughty"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if acl.Check("alice", net.ParseIP("203.0.113.5")) {
		t.Error("global block should reject in /24")
	}
	if !acl.Check("alice", net.ParseIP("203.0.114.5")) {
		t.Error("IP outside global block should still pass")
	}
}

func TestACLPerTunnelAllowlistLocksDown(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("alice", "allow", "10.0.0.0/8", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Tunnel "alice" only allows 10.0.0.0/8.
	if !acl.Check("alice", net.ParseIP("10.1.2.3")) {
		t.Error("10/8 should pass alice's allowlist")
	}
	if acl.Check("alice", net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 should fail alice's allowlist")
	}
	// Other tunnels (no rules) are unaffected.
	if !acl.Check("bob", net.ParseIP("8.8.8.8")) {
		t.Error("bob has no rules — 8.8.8.8 should pass")
	}
}

func TestACLPerTunnelBlockBeatsAllow(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("alice", "allow", "10.0.0.0/8", ""); err != nil {
		t.Fatal(err)
	}
	if err := acl.Add("alice", "block", "10.1.2.3", ""); err != nil {
		t.Fatal(err)
	}
	if acl.Check("alice", net.ParseIP("10.1.2.3")) {
		t.Error("explicit block must win over allowlist match")
	}
}

func TestACLGlobalAllowlistAppliesWhenNoTunnelRules(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("global", "allow", "10.0.0.0/8", ""); err != nil {
		t.Fatal(err)
	}
	if !acl.Check("alice", net.ParseIP("10.0.0.1")) {
		t.Error("global allowlist match should permit")
	}
	if acl.Check("alice", net.ParseIP("8.8.8.8")) {
		t.Error("global allowlist non-empty + no match → deny")
	}
}

func TestACLAddRejectsDuplicate(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("alice", "allow", "10.0.0.0/8", ""); err != nil {
		t.Fatal(err)
	}
	if err := acl.Add("alice", "allow", "10.0.0.0/8", ""); err == nil {
		t.Error("expected duplicate to be rejected")
	}
}

func TestACLAddRejectsBadScopeAndList(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("Bad.Scope", "allow", "10.0.0.1", ""); err == nil {
		t.Error("invalid scope should be rejected")
	}
	if err := acl.Add("alice", "weird", "10.0.0.1", ""); err == nil {
		t.Error("invalid list should be rejected")
	}
}

func TestACLRemove(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("alice", "allow", "10.0.0.1", ""); err != nil {
		t.Fatal(err)
	}
	if err := acl.Remove("alice", "allow", "10.0.0.1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Removing again must fail.
	if err := acl.Remove("alice", "allow", "10.0.0.1"); err == nil {
		t.Error("expected error removing absent entry")
	}
	// Tunnel should be dropped from configured list once empty.
	if got := acl.TunnelsConfigured(); len(got) != 0 {
		t.Errorf("expected no configured tunnels, got %v", got)
	}
}

func TestACLRemoveUnknownScope(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Remove("never-configured", "allow", "10.0.0.1"); err == nil {
		t.Error("expected error removing from unknown scope")
	}
}

func TestACLSnapshotsAreIsolated(t *testing.T) {
	acl := mustACL(t)
	if err := acl.Add("global", "allow", "10.0.0.0/8", ""); err != nil {
		t.Fatal(err)
	}
	snap := acl.SnapshotGlobal()
	snap.Allowed = nil
	// Mutating snapshot must not affect live state.
	if got := acl.SnapshotGlobal(); len(got.Allowed) != 1 {
		t.Errorf("snapshot mutation leaked, got %+v", got)
	}
}

func TestACLPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acl.json")
	acl, err := newIPACL(path)
	if err != nil {
		t.Fatalf("first new: %v", err)
	}
	if err := acl.Add("alice", "allow", "10.0.0.0/8", "lan"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Reload and verify.
	acl2, err := newIPACL(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !acl2.Check("alice", net.ParseIP("10.1.2.3")) {
		t.Error("persisted rule didn't take effect after reload")
	}
	if got := acl2.TunnelsConfigured(); len(got) != 1 || got[0] != "alice" {
		t.Errorf("expected [alice], got %v", got)
	}
}

func TestACLLoadMigratesLegacyFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "acl.json")
	legacy := `{
  "allowed": [{"raw": "10.0.0.0/8"}],
  "blocked": [{"raw": "203.0.113.5"}]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	acl, err := newIPACL(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if acl.Check("any", net.ParseIP("203.0.113.5")) {
		t.Error("legacy blocklist not promoted to global")
	}
	if !acl.Check("any", net.ParseIP("10.0.0.1")) {
		t.Error("legacy allowlist not promoted (10/8 must match)")
	}
	// File should have been rewritten without the legacy keys.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := parsed["allowed"]; ok {
		t.Error("expected legacy 'allowed' key to be gone after migration")
	}
	if _, ok := parsed["blocked"]; ok {
		t.Error("expected legacy 'blocked' key to be gone after migration")
	}
}

func TestDirOf(t *testing.T) {
	if got := dirOf("/var/lib/mygrok/acl.json"); got != "/var/lib/mygrok" {
		t.Errorf("got %q", got)
	}
	if got := dirOf("acl.json"); got != "." {
		t.Errorf("got %q", got)
	}
}

func mustACL(t *testing.T) *ipACL {
	t.Helper()
	acl, err := newIPACL(filepath.Join(t.TempDir(), "acl.json"))
	if err != nil {
		t.Fatalf("new acl: %v", err)
	}
	return acl
}
