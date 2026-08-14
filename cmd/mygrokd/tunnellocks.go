package main

// Per-tunnel passkey access lists. A tunnel with a non-empty
// allowed_users entry is "locked": only those users' credentials can
// pass the gate. A tunnel with no entry (or an empty list) is open.
//
// Persisted to JSON. Old single-bool {"locked":[...]} files are
// migrated to {"tunnels":{<sub>:{"allowed_users":[<all-existing-user-ids>]}}}
// on first load.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

type tunnelLockEntry struct {
	AllowedUsers []string `json:"allowed_users"`
}

type tunnelLocksFile struct {
	Tunnels map[string]tunnelLockEntry `json:"tunnels,omitempty"`

	// Legacy single-bool list, migrated on load.
	LegacyLocked []string `json:"locked,omitempty"`
}

type tunnelLocks struct {
	mu   sync.RWMutex
	path string
	// allowed[sub] = set of user_ids permitted on this tunnel.
	allowed map[string]map[string]bool
}

func newTunnelLocks(path string) (*tunnelLocks, error) {
	l := &tunnelLocks{path: path, allowed: map[string]map[string]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, err
	}
	var f tunnelLocksFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for sub, e := range f.Tunnels {
		set := map[string]bool{}
		for _, u := range e.AllowedUsers {
			set[u] = true
		}
		l.allowed[sub] = set
	}
	// Legacy entries — we don't know which user(s) to grant, so we
	// leave the entries with empty allowed_users which the gate
	// treats as "locked but no one allowed". Admin is expected to
	// run grant <sub> <user> after upgrading. We log a one-time
	// warning so this is visible in journalctl.
	for _, sub := range f.LegacyLocked {
		if _, ok := l.allowed[sub]; !ok {
			l.allowed[sub] = map[string]bool{}
			fmt.Fprintf(os.Stderr, "tunnellocks: migrated legacy lock for %q with no allowed users — run `mygrok admin grant %s <user>` to restore access\n", sub, sub)
		}
	}
	if len(f.LegacyLocked) > 0 {
		l.mu.Lock()
		_ = l.saveLocked()
		l.mu.Unlock()
	}
	return l, nil
}

func (l *tunnelLocks) saveLocked() error {
	if l.path == "" {
		return nil
	}
	if err := os.MkdirAll(dirOf(l.path), 0o755); err != nil {
		return err
	}
	out := tunnelLocksFile{Tunnels: map[string]tunnelLockEntry{}}
	for sub, set := range l.allowed {
		users := make([]string, 0, len(set))
		for u := range set {
			users = append(users, u)
		}
		sort.Strings(users)
		out.Tunnels[sub] = tunnelLockEntry{AllowedUsers: users}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// IsLocked reports whether the tunnel has an active access list.
func (l *tunnelLocks) IsLocked(sub string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.allowed[sub]
	return ok
}

// AllowsUser reports whether userID is in the tunnel's allow list.
func (l *tunnelLocks) AllowsUser(sub, userID string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	set, ok := l.allowed[sub]
	if !ok {
		return false
	}
	return set[userID]
}

// AllowedUsers returns the set of user_ids permitted on a tunnel.
func (l *tunnelLocks) AllowedUsers(sub string) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	set := l.allowed[sub]
	out := make([]string, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Grant adds userID to the tunnel's allow list, locking the tunnel if
// it wasn't already.
func (l *tunnelLocks) Grant(sub, userID string) error {
	if !looksLikeSubdomain(sub) {
		return errInvalidSubdomain
	}
	if userID == "" {
		return fmt.Errorf("user_id required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.allowed[sub] == nil {
		l.allowed[sub] = map[string]bool{}
	}
	l.allowed[sub][userID] = true
	return l.saveLocked()
}

// Revoke removes userID from the tunnel's allow list. If the list is
// empty afterwards, the tunnel is fully unlocked.
func (l *tunnelLocks) Revoke(sub, userID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	set, ok := l.allowed[sub]
	if !ok {
		return fmt.Errorf("tunnel %q is not locked", sub)
	}
	if !set[userID] {
		return fmt.Errorf("user %q is not on %q's allow list", userID, sub)
	}
	delete(set, userID)
	if len(set) == 0 {
		delete(l.allowed, sub)
	}
	return l.saveLocked()
}

// Unlock removes the entire access list for a tunnel.
func (l *tunnelLocks) Unlock(sub string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.allowed[sub]; !ok {
		return nil
	}
	delete(l.allowed, sub)
	return l.saveLocked()
}

// RevokeUserAcrossTunnels strips a user from every tunnel they're on
// (used when a user is deleted). Returns the list of subs touched.
func (l *tunnelLocks) RevokeUserAcrossTunnels(userID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var touched []string
	for sub, set := range l.allowed {
		if set[userID] {
			delete(set, userID)
			touched = append(touched, sub)
			if len(set) == 0 {
				delete(l.allowed, sub)
			}
		}
	}
	if len(touched) > 0 {
		_ = l.saveLocked()
	}
	return touched
}

// LockedSubdomains returns sorted subdomains that have an access list.
func (l *tunnelLocks) LockedSubdomains() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.allowed))
	for s := range l.allowed {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

var errInvalidSubdomain = errInvalidSubdomainConst("invalid subdomain")

type errInvalidSubdomainConst string

func (e errInvalidSubdomainConst) Error() string { return string(e) }
