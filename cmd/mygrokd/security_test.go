package main

import (
	"strings"
	"testing"
)

func TestSafeReturnURL(t *testing.T) {
	const public = "example.com"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"root-relative path is fine", "/dashboard?x=1", "/dashboard?x=1"},
		{"absolute https under the zone", "https://app.example.com/x", "https://app.example.com/x"},
		{"the zone apex itself", "https://example.com/", "https://example.com/"},

		// The whole point: none of these may survive.
		{"offsite https", "https://evil.com/", "/"},
		{"lookalike suffix", "https://notexample.com/", "/"},
		{"lookalike prefix", "https://example.com.evil.com/", "/"},
		{"protocol-relative", "//evil.com/", "/"},
		{"backslash protocol-relative", `/\evil.com/`, "/"},
		{"javascript URI", "javascript:alert(document.domain)", "/"},
		{"uppercase javascript URI", "JavaScript:alert(1)", "/"},
		{"data URI", "data:text/html,<script>alert(1)</script>", "/"},
		{"plain http under the zone", "http://app.example.com/", "/"},
		{"scheme-less host", "evil.com", "/"},
		{"leading whitespace is trimmed, then judged", "   https://evil.com/", "/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := safeReturnURL(c.in, public); got != c.want {
				t.Errorf("safeReturnURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSafeReturnURLIsCaseInsensitiveOnHost(t *testing.T) {
	got := safeReturnURL("https://APP.EXAMPLE.COM/x", "example.com")
	if got != "https://APP.EXAMPLE.COM/x" {
		t.Errorf("got %q, want the URL preserved", got)
	}
	if safeReturnURL("https://APP.EVIL.COM/x", "example.com") != "/" {
		t.Error("an offsite host must be rejected regardless of case")
	}
}

func TestStripMygrokCookies(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means the whole header should be dropped
	}{
		{
			name: "session cookie removed, others kept",
			in:   "Cookie: a=1; mygrok_pk=secret; b=2",
			want: "Cookie: a=1; b=2",
		},
		{
			name: "admin cookie removed",
			in:   "Cookie: mygrok_admin=sid; theme=dark",
			want: "Cookie: theme=dark",
		},
		{
			name: "all the in-flight ones",
			in:   "Cookie: mygrok_pk_login=x; mygrok_pk_reg=y; keep=z",
			want: "Cookie: keep=z",
		},
		{
			name: "nothing of ours, untouched",
			in:   "Cookie: session=abc; csrf=def",
			want: "Cookie: session=abc; csrf=def",
		},
		{
			name: "only ours means drop the header entirely",
			in:   "Cookie: mygrok_pk=secret",
			want: "",
		},
		{
			// A prefix match would wrongly drop an application's own cookie.
			name: "similar names are not ours",
			in:   "Cookie: mygrok_pk_custom=keep; mygrok_pkx=keep2",
			want: "Cookie: mygrok_pk_custom=keep; mygrok_pkx=keep2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripMygrokCookies([]byte(c.in))
			if c.want == "" {
				if got != nil {
					t.Errorf("got %q, want the header dropped", got)
				}
				return
			}
			if string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripMygrokCookiesNeverLeaksSecret(t *testing.T) {
	// Belt and braces: whatever the shape of the header, the value must not
	// survive into what we forward.
	for _, in := range []string{
		"Cookie: mygrok_pk=SECRETVALUE",
		"Cookie: x=1; mygrok_pk=SECRETVALUE; y=2",
		"Cookie:mygrok_pk=SECRETVALUE",
		"cookie: mygrok_pk=SECRETVALUE; z=3",
	} {
		got := string(stripMygrokCookies([]byte(in)))
		if strings.Contains(got, "SECRETVALUE") {
			t.Errorf("%q leaked the session value: %q", in, got)
		}
	}
}

func TestSameOriginPOST(t *testing.T) {
	const public = "example.com"
	cases := []struct {
		name    string
		headers string
		want    bool
	}{
		{
			// curl and `mygrok admin` send neither header. They are not the
			// threat: a cross-site attacker can't suppress both from a browser.
			name:    "no Origin and no Referer is allowed",
			headers: "POST /admin/ips HTTP/1.1\r\nHost: tunnel.example.com\r\n\r\n",
			want:    true,
		},
		{
			name:    "Origin is the management host",
			headers: "POST /admin/ips HTTP/1.1\r\nOrigin: https://tunnel.example.com\r\n\r\n",
			want:    true,
		},
		{
			name:    "Referer is the management host",
			headers: "POST /admin/ips HTTP/1.1\r\nReferer: https://tunnel.example.com/admin/ips\r\n\r\n",
			want:    true,
		},
		{
			name:    "Origin elsewhere is rejected",
			headers: "POST /admin/ips HTTP/1.1\r\nOrigin: https://evil.com\r\n\r\n",
			want:    false,
		},
		{
			// A tunnel on the same zone is still a different origin, and its
			// operator is exactly who shouldn't be able to drive the admin UI.
			name:    "another tunnel on the same zone is rejected",
			headers: "POST /admin/ips HTTP/1.1\r\nOrigin: https://app.example.com\r\n\r\n",
			want:    false,
		},
		{
			name:    "Origin wins over Referer",
			headers: "POST /admin/ips HTTP/1.1\r\nOrigin: https://evil.com\r\nReferer: https://tunnel.example.com/\r\n\r\n",
			want:    false,
		},
		{
			name:    "Origin: null is rejected",
			headers: "POST /admin/ips HTTP/1.1\r\nOrigin: null\r\n\r\n",
			want:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sameOriginPOST([]byte(c.headers), public); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDefaultCertDomainsNeverWildcardWithoutDNS(t *testing.T) {
	// Guard the invariant setupTLS relies on: a wildcard needs DNS-01, so
	// the no-provider defaults must never contain one.
	for _, host := range []string{"example.com", "t.example.com", "example.co.uk"} {
		for _, d := range defaultCertDomains(host, false) {
			if strings.HasPrefix(d, "*.") {
				t.Errorf("defaultCertDomains(%q, false) returned wildcard %q", host, d)
			}
		}
	}
}
