package main

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseBasicAuthFlag(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty disables auth", "", "", false},
		{"whitespace only disables auth", "   ", "", false},
		{"valid user:pass", "alice:s3cret", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret")), false},
		{"trimmed", "  bob:hunter2  ", "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:hunter2")), false},
		{"empty password still parses", "user:", "Basic " + base64.StdEncoding.EncodeToString([]byte("user:")), false},
		{"missing colon", "justusername", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBasicAuthFlag(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestBasicAuthOK(t *testing.T) {
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))

	if !basicAuthOK("anything", "") {
		t.Error("empty expected should always allow")
	}
	if !basicAuthOK(expected, expected) {
		t.Error("matching credentials should pass")
	}
	if basicAuthOK("Basic "+base64.StdEncoding.EncodeToString([]byte("user:wrong")), expected) {
		t.Error("wrong password should fail")
	}
	if basicAuthOK("", expected) {
		t.Error("missing header should fail")
	}
	if basicAuthOK("Bearer something", expected) {
		t.Error("non-basic scheme should fail")
	}
}

func TestRequireBasicAuthPassthroughWhenDisabled(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	h := requireBasicAuth(next, "")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("handler should have been called")
	}
	if rr.Code != 200 {
		t.Errorf("got %d want 200", rr.Code)
	}
}

func TestRequireBasicAuthChallengeAndAllow(t *testing.T) {
	expected := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	})
	h := requireBasicAuth(next, expected)

	// Missing Authorization → 401 challenge with WWW-Authenticate.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("got %d want 401", rr.Code)
	}
	if !strings.HasPrefix(rr.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("missing WWW-Authenticate challenge, got %q", rr.Header().Get("WWW-Authenticate"))
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("expected Cache-Control no-store, got %q", rr.Header().Get("Cache-Control"))
	}

	// Wrong password → 401.
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("u:nope")))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong creds: got %d want 401", rr.Code)
	}

	// Correct password → 200.
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", expected)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Errorf("good creds: got %d want 200", rr.Code)
	}
	if rr.Body.String() != "ok" {
		t.Errorf("got body %q want %q", rr.Body.String(), "ok")
	}
}
