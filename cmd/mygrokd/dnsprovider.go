package main

// DNS provider selection.
//
// mygrokd uses a DNS provider for two things, both optional:
//
//   1. Solving the ACME DNS-01 challenge, which is the only way to get a
//      *wildcard* certificate for *.<publicHost>. One wildcard covers every
//      tunnel, so a new subdomain is instantly serving TLS with no ACME
//      round-trip and no rate-limit exposure.
//   2. Writing the per-tunnel A record that LAN-direct needs.
//
// Neither is mandatory. With --dns-provider=none (the default) the server
// still serves HTTPS on every tunnel: certmagic issues a certificate per
// hostname on demand over TLS-ALPN-01, which works because every
// <sub>.<publicHost> already resolves to this box via wildcard DNS. The
// trade-offs are a one-off ~1s stall on a subdomain's first HTTPS request
// and Let's Encrypt's per-domain certificate rate limits, which is a fine
// deal for a personal or small-team server and needs no cloud credentials
// at all.

import (
	"fmt"
	"os"
	"strings"

	"github.com/libdns/cloudflare"
	"github.com/libdns/digitalocean"
	"github.com/libdns/libdns"
	"github.com/libdns/route53"
)

// dnsProvider is the slice of libdns a provider must implement to serve
// both jobs: append/delete for the ACME challenge, set/delete for the
// LAN-direct A records.
type dnsProvider interface {
	libdns.RecordAppender
	libdns.RecordDeleter
	libdns.RecordSetter
}

// dnsProviderNames lists what --dns-provider accepts, for help text and
// error messages.
var dnsProviderNames = []string{"none", "route53", "cloudflare", "digitalocean"}

// buildDNSProvider resolves --dns-provider into a live provider. It
// returns (nil, nil) for "none", which callers must treat as "wildcard
// certs and LAN-direct are off", not as an error.
//
// Credentials come from the environment in every case, so they can live in
// a root-only systemd EnvironmentFile and never touch the command line
// (where they'd show up in `ps`).
func buildDNSProvider(name string) (dnsProvider, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "none", "off":
		return nil, nil

	case "route53", "aws":
		// The AWS SDK's default credential chain: AWS_ACCESS_KEY_ID /
		// AWS_SECRET_ACCESS_KEY, a shared profile, or the instance role.
		// An EC2 instance role means no static keys on the box at all, so
		// we can't usefully pre-validate here.
		return &route53.Provider{}, nil

	case "cloudflare":
		token := os.Getenv("CLOUDFLARE_API_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN not set " +
				"(needs a token with Zone:DNS:Edit on the zone)")
		}
		return &cloudflare.Provider{APIToken: token}, nil

	case "digitalocean":
		token := os.Getenv("DO_AUTH_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("DO_AUTH_TOKEN not set " +
				"(needs a DigitalOcean personal access token with write scope)")
		}
		return &digitalocean.Provider{APIToken: token}, nil

	default:
		return nil, fmt.Errorf("unknown --dns-provider %q (want one of: %s)",
			name, strings.Join(dnsProviderNames, ", "))
	}
}

// defaultCertDomains picks what to request a certificate for when
// --cert-domains isn't given.
//
// With a DNS provider we take the wildcard, which covers every tunnel at
// once. Without one, DNS-01 is unavailable, so we only pre-issue for the
// management host and let everything else arrive on demand.
func defaultCertDomains(publicHost string, hasDNS bool) []string {
	if hasDNS {
		return []string{"*." + publicHost, publicHost}
	}
	return []string{"tunnel." + publicHost}
}
