package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestProxyToLocalLogsRequests fires several requests through proxyToLocal and
// asserts the per-request log lines (method, status, duration) appear.
func TestProxyToLocalLogsRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "hi")
		case "/api/users":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":1}`)
		case "/missing":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "nope")
		case "/bad":
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "boom")
		default:
			w.WriteHeader(204)
		}
	}))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")

	var logBuf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(&teeWriter{w: &logBuf, mu: &mu})
	log.SetFlags(0) // deterministic output for assertions
	defer func() { log.SetOutput(nil); log.SetFlags(log.LstdFlags) }()

	clientSide, serverSide := pairTCP(t)
	defer clientSide.Close()

	done := make(chan struct{})
	go func() {
		proxyToLocal(serverSide, target)
		close(done)
	}()

	br := bufio.NewReader(clientSide)
	cases := []struct {
		path string
		want int
	}{
		{"/", 200},
		{"/api/users", 201},
		{"/missing", 404},
		{"/bad", 500},
	}
	for _, c := range cases {
		_, err := fmt.Fprintf(clientSide, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\n\r\n", c.path)
		if err != nil {
			t.Fatalf("write %s: %v", c.path, err)
		}
		resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatalf("read response %s: %v", c.path, err)
		}
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d want %d", c.path, resp.StatusCode, c.want)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Trigger end-of-stream so proxyToLocal returns.
	_ = clientSide.(interface{ CloseWrite() error }).CloseWrite()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyToLocal did not return")
	}

	mu.Lock()
	out := logBuf.String()
	mu.Unlock()

	for _, want := range []string{
		"GET    200 OK",
		"GET    201 Created",
		"GET    404 Not Found",
		"GET    500 Internal Server Error",
		"  /",
		"  /api/users",
		"  /missing",
		"  /bad",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// TestProxyToLocalDialFailureLogged covers the case where the local backend
// isn't reachable: the caller should still see a 502 and the failure logged.
func TestProxyToLocalDialFailureLogged(t *testing.T) {
	// Pick a port that's almost certainly closed.
	target := "127.0.0.1:1" // privileged + nothing listening for our user

	var logBuf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(&teeWriter{w: &logBuf, mu: &mu})
	log.SetFlags(0)
	defer func() { log.SetOutput(nil); log.SetFlags(log.LstdFlags) }()

	clientSide, serverSide := pairTCP(t)
	defer clientSide.Close()

	done := make(chan struct{})
	go func() {
		proxyToLocal(serverSide, target)
		close(done)
	}()

	_, _ = fmt.Fprintf(clientSide, "POST /v1/things HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n")
	br := bufio.NewReader(clientSide)
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 502 {
		t.Errorf("got %d want 502", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("proxyToLocal did not return after dial failure")
	}

	mu.Lock()
	out := logBuf.String()
	mu.Unlock()

	for _, want := range []string{"POST", "502", "/v1/things"} {
		if !strings.Contains(out, want) {
			t.Errorf("dial-failure log missing %q\n--- log ---\n%s", want, out)
		}
	}
}

// TestProxyToLocalSurvivesBackendCloseBetweenRequests is the regression test
// for the keep-alive race that produced "unexpected EOF" / "read: connection
// reset" log lines: with the old design, all requests on one yamux stream
// shared one local TCP connection, so once the local server hung up that
// socket (Puma's persistent_timeout, a `Connection: close` response, etc.),
// every subsequent request on the same stream failed.
//
// We simulate that by responding with `Connection: close` on every request,
// which forces the backend to hang up after each response. All three
// pipelined requests on the same yamux stream must still receive a
// well-formed 200 response.
func TestProxyToLocalSurvivesBackendCloseBetweenRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")

	log.SetOutput(io.Discard)
	log.SetFlags(0)
	defer func() { log.SetOutput(nil); log.SetFlags(log.LstdFlags) }()

	clientSide, serverSide := pairTCP(t)
	defer clientSide.Close()

	done := make(chan struct{})
	go func() {
		proxyToLocal(serverSide, target)
		close(done)
	}()

	br := bufio.NewReader(clientSide)
	for _, path := range []string{"/a", "/b", "/c"} {
		if _, err := fmt.Fprintf(clientSide, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\n\r\n", path); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatalf("read response %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: got status %d want 200", path, resp.StatusCode)
		}
		if string(body) != path {
			t.Errorf("%s: got body %q want %q", path, body, path)
		}
	}

	_ = clientSide.(interface{ CloseWrite() error }).CloseWrite()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxyToLocal did not return")
	}
}

// pairTCP returns a connected pair of *net.TCPConns. We use a real loopback
// listener (rather than net.Pipe) because proxyToLocal calls SetReadDeadline,
// which net.Pipe doesn't support.
func pairTCP(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-ch
	if server == nil {
		t.Fatal("accept returned nil")
	}
	return client, server
}

type teeWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.w.Write(p)
}
