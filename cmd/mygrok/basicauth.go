package main

// HTTP basic auth, applied at the mygrok agent. Same flag works for
// `mygrok http` (checked in forwardOne before the request reaches the
// local backend) and `mygrok serve` (wraps the static handler so it
// covers both standalone and tunnel modes).

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// parseBasicAuthFlag turns "user:pass" into the precomputed Authorization
// header value an authorized client must send: `Basic <base64(user:pass)>`.
// Returns "" when the flag is empty (auth disabled). Pre-computing once at
// startup avoids re-encoding on every request.
// resolveBasicAuth picks the credentials to enforce: the --basic-auth flag
// if given, otherwise MYGROK_BASIC_AUTH.
//
// The environment variable exists so `mygrok service install` never has to
// put a password in a service file or in argv. A systemd unit in
// /etc/systemd/system is world-readable by convention, and argv is visible
// to every user on the box via ps — so the installed service passes the
// credentials through its (0600) environment instead.
func resolveBasicAuth(flagValue string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("MYGROK_BASIC_AUTH"))
}

func parseBasicAuthFlag(flag string) (string, error) {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return "", nil
	}
	if !strings.Contains(flag, ":") {
		return "", fmt.Errorf("--basic-auth must be in the form user:pass")
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(flag)), nil
}

// basicAuthOK constant-time-compares an inbound Authorization header
// value against the precomputed expected. expected == "" means auth is
// disabled (always allows).
func basicAuthOK(actual, expected string) bool {
	if expected == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

// requireBasicAuth wraps an http.Handler with basic auth. Used by
// `mygrok serve` so the static handler enforces auth in both standalone
// and tunnel modes.
func requireBasicAuth(next http.Handler, expected string) http.Handler {
	if expected == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !basicAuthOK(r.Header.Get("Authorization"), expected) {
			w.Header().Set("WWW-Authenticate", `Basic realm="mygrok"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
