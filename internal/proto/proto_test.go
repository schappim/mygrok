package proto

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestWriteJSONLineAppendsNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSONLine(&buf, Hello{Version: "1", Subdomain: "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected trailing newline, got %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one newline, got %q", out)
	}
	if !strings.Contains(out, `"version":"1"`) {
		t.Errorf("payload missing version field: %q", out)
	}
}

func TestRoundtripHello(t *testing.T) {
	in := Hello{
		Version:   Version,
		Auth:      "secret",
		Subdomain: "demo",
		Proto:     "http",
		ClientID:  "abc123",
		Hostnames: []string{"a.example.com", "b.example.com"},
		LANIP:     "192.168.1.10",
		LANPort:   8443,
	}
	var buf bytes.Buffer
	if err := WriteJSONLine(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out Hello
	if err := ReadJSONLine(bufio.NewReader(&buf), &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.Version != in.Version || out.Auth != in.Auth || out.Subdomain != in.Subdomain ||
		out.Proto != in.Proto || out.ClientID != in.ClientID || out.LANIP != in.LANIP ||
		out.LANPort != in.LANPort || len(out.Hostnames) != len(in.Hostnames) {
		t.Fatalf("roundtrip mismatch:\ngot  %+v\nwant %+v", out, in)
	}
	for i := range in.Hostnames {
		if out.Hostnames[i] != in.Hostnames[i] {
			t.Errorf("Hostnames[%d]: got %q want %q", i, out.Hostnames[i], in.Hostnames[i])
		}
	}
}

func TestRoundtripHelloResp(t *testing.T) {
	in := HelloResp{
		OK:          true,
		URL:         "https://demo.example.com",
		URLs:        []string{"https://demo.example.com", "http://demo.example.com"},
		LANHostname: "demo-lan.example.com",
		LANCertPEM:  "-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----",
		LANKeyPEM:   "-----BEGIN PRIVATE KEY-----\nfake\n-----END PRIVATE KEY-----",
	}
	var buf bytes.Buffer
	if err := WriteJSONLine(&buf, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out HelloResp
	if err := ReadJSONLine(bufio.NewReader(&buf), &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if out.OK != in.OK || out.URL != in.URL || out.LANHostname != in.LANHostname ||
		out.LANCertPEM != in.LANCertPEM || out.LANKeyPEM != in.LANKeyPEM ||
		len(out.URLs) != len(in.URLs) {
		t.Fatalf("roundtrip mismatch:\ngot  %+v\nwant %+v", out, in)
	}
	for i := range in.URLs {
		if out.URLs[i] != in.URLs[i] {
			t.Errorf("URLs[%d]: got %q want %q", i, out.URLs[i], in.URLs[i])
		}
	}
}

func TestReadJSONLineOmitsTrailingFieldsWhenEmpty(t *testing.T) {
	// Server replies with just OK + URL; client must not crash on missing
	// optional fields.
	var buf bytes.Buffer
	if err := WriteJSONLine(&buf, HelloResp{OK: true, URL: "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := buf.String()
	for _, banned := range []string{"lan_cert_pem", "lan_key_pem", "lan_hostname", "error", "urls"} {
		if strings.Contains(got, banned) {
			t.Errorf("expected %q to be omitted, got %s", banned, got)
		}
	}
}

func TestReadJSONLineEOF(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	var out Hello
	err := ReadJSONLine(r, &out)
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestReadJSONLineMalformed(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{not json\n"))
	var out Hello
	if err := ReadJSONLine(r, &out); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

func TestReadJSONLineStopsAtNewline(t *testing.T) {
	// Two JSON objects back to back. Reader should consume only the first.
	src := `{"version":"1","subdomain":"a"}` + "\n" + `{"version":"1","subdomain":"b"}` + "\n"
	r := bufio.NewReader(strings.NewReader(src))
	var first Hello
	if err := ReadJSONLine(r, &first); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.Subdomain != "a" {
		t.Errorf("first subdomain: got %q want %q", first.Subdomain, "a")
	}
	var second Hello
	if err := ReadJSONLine(r, &second); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second.Subdomain != "b" {
		t.Errorf("second subdomain: got %q want %q", second.Subdomain, "b")
	}
}
