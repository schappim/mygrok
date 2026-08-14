package main

import (
	"strings"
	"testing"
)

func TestBuildDNSProviderNone(t *testing.T) {
	// "none" must be a clean nil, not an error — it's the default, and
	// callers key wildcard-cert and LAN-direct behaviour off nil-ness.
	for _, name := range []string{"none", "", "  ", "NONE", "off"} {
		p, err := buildDNSProvider(name)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", name, err)
		}
		if p != nil {
			t.Errorf("%q: got a provider, want nil", name)
		}
	}
}

func TestBuildDNSProviderRoute53(t *testing.T) {
	// Route53 resolves credentials lazily through the AWS chain (env,
	// profile, or instance role), so construction must succeed with no
	// env set rather than guessing that credentials are missing.
	for _, name := range []string{"route53", "aws", "Route53"} {
		p, err := buildDNSProvider(name)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", name, err)
		}
		if p == nil {
			t.Fatalf("%q: got nil provider", name)
		}
	}
}

func TestBuildDNSProviderTokenProviders(t *testing.T) {
	cases := []struct {
		provider string
		envVar   string
	}{
		{"cloudflare", "CLOUDFLARE_API_TOKEN"},
		{"digitalocean", "DO_AUTH_TOKEN"},
	}
	for _, c := range cases {
		t.Run(c.provider, func(t *testing.T) {
			t.Setenv(c.envVar, "")
			if _, err := buildDNSProvider(c.provider); err == nil {
				t.Fatal("expected an error when the token env var is unset")
			} else if !strings.Contains(err.Error(), c.envVar) {
				t.Errorf("error should name %s, got: %v", c.envVar, err)
			}

			t.Setenv(c.envVar, "token-value")
			p, err := buildDNSProvider(c.provider)
			if err != nil {
				t.Fatalf("unexpected error with token set: %v", err)
			}
			if p == nil {
				t.Fatal("got nil provider with token set")
			}
		})
	}
}

func TestBuildDNSProviderUnknown(t *testing.T) {
	_, err := buildDNSProvider("azure")
	if err == nil {
		t.Fatal("expected an error for an unsupported provider")
	}
	// The message should list the valid options; a bad --dns-provider is
	// the kind of typo that shouldn't send anyone to the source.
	for _, name := range dnsProviderNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should list %q, got: %v", name, err)
		}
	}
}

func TestDefaultCertDomains(t *testing.T) {
	// With DNS-01 available, one wildcard covers every tunnel.
	got := defaultCertDomains("example.com", true)
	want := []string{"*.example.com", "example.com"}
	if len(got) != len(want) {
		t.Fatalf("with DNS: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("with DNS: [%d] got %q, want %q", i, got[i], want[i])
		}
	}

	// Without it, only the management host is pre-issued; tunnels get
	// certificates on demand over TLS-ALPN-01.
	got = defaultCertDomains("example.com", false)
	if len(got) != 1 || got[0] != "tunnel.example.com" {
		t.Errorf("without DNS: got %v, want [tunnel.example.com]", got)
	}
	for _, d := range got {
		if strings.HasPrefix(d, "*.") {
			t.Errorf("without DNS the defaults must not include a wildcard: %q", d)
		}
	}
}
