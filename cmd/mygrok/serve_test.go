package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/schappim/mygrok/internal/branding"
)

func TestDiskHasFavicon(t *testing.T) {
	dir := t.TempDir()
	if diskHasFavicon(dir) {
		t.Error("empty dir should not have favicon")
	}
	if err := os.WriteFile(filepath.Join(dir, "favicon.ico"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !diskHasFavicon(dir) {
		t.Error("favicon.ico should be detected")
	}

	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "favicon.svg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !diskHasFavicon(dir2) {
		t.Error("favicon.svg should be detected")
	}
}

func TestStaticHandlerFallsBackToBrandingFavicon(t *testing.T) {
	dir := t.TempDir()
	h := staticHandler(dir, "index.html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/favicon.ico", nil))
	if rr.Code != 200 {
		t.Errorf("got %d want 200", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("content-type: got %q", got)
	}
	if rr.Body.String() != branding.FaviconSVG {
		t.Errorf("expected branding favicon SVG")
	}

	// SVG fallback path too.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/favicon.svg", nil))
	if rr.Code != 200 {
		t.Errorf("got %d want 200", rr.Code)
	}
	if rr.Body.String() != branding.FaviconSVG {
		t.Error("expected branding SVG for /favicon.svg")
	}
}

func TestStaticHandlerServesUserFaviconWhenPresent(t *testing.T) {
	dir := t.TempDir()
	custom := []byte("my-custom-icon-bytes")
	if err := os.WriteFile(filepath.Join(dir, "favicon.ico"), custom, 0o644); err != nil {
		t.Fatal(err)
	}
	h := staticHandler(dir, "index.html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/favicon.ico", nil))
	if rr.Code != 200 {
		t.Errorf("got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "my-custom-icon-bytes") {
		t.Errorf("expected user favicon to be served, got %q", rr.Body.String())
	}
}

func TestStaticHandlerCustomIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "home.html"), []byte("<h1>home</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default index.html → directory listing because home.html isn't recognised.
	h := staticHandler(dir, "home.html")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
	if rr.Code != 200 {
		t.Errorf("got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "home") {
		t.Errorf("custom index not served: %q", rr.Body.String())
	}
}

func TestStaticHandlerServesFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hi.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := staticHandler(dir, "index.html")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/hi.txt", nil))
	if rr.Code != 200 {
		t.Errorf("got %d", rr.Code)
	}
	if rr.Body.String() != "hello" {
		t.Errorf("got body %q", rr.Body.String())
	}
}

func TestStatusRecorderOnlyCapturesFirstWriteHeader(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: rr, status: 200}
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusInternalServerError) // should be ignored
	if rec.status != http.StatusCreated {
		t.Errorf("got %d want %d", rec.status, http.StatusCreated)
	}
}
