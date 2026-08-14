// Package proto defines the framing used between mygrok client and server
// before yamux multiplexing takes over.
package proto

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
)

const Version = "1"

type Hello struct {
	Version   string `json:"version"`
	Auth      string `json:"auth"`
	Subdomain string `json:"subdomain"`
	Proto     string `json:"proto"` // "http"
	// ClientID is a per-process random ID. The server uses it to tell
	// "same client reconnecting" (silent takeover) apart from "two clients
	// fighting over the same subdomain" (rejected).
	ClientID string `json:"client_id,omitempty"`
	// Hostnames are additional public hostnames (typically CNAMEd to the
	// tunnel server) that the client wants this tunnel to also accept.
	// The server validates each, registers them in the routing table,
	// and uses certmagic's OnDemand to issue TLS certs lazily.
	Hostnames []string `json:"hostnames,omitempty"`
	// LANIP is an RFC1918 IPv4 address the client wants advertised in
	// public DNS at <sub>-lan.<publicHost>. Visitors on the same LAN
	// (same public IP as the client) get 307'd to that hostname so the
	// hot path bypasses the tunnel server entirely. Empty = LAN-direct
	// disabled (opt-in via the client's --lan flag).
	LANIP string `json:"lan_ip,omitempty"`
	// LANPort is the TLS port the client will serve on at LANIP.
	LANPort int `json:"lan_port,omitempty"`
}

type HelloResp struct {
	OK    bool     `json:"ok"`
	URL   string   `json:"url,omitempty"`  // primary public URL
	URLs  []string `json:"urls,omitempty"` // all public URLs (https first if available)
	Error string   `json:"error,omitempty"`
	// LAN-direct fields. Populated only when the client requested it
	// (via Hello.LANIP) and the server successfully wrote the public
	// A record + has a wildcard cert to ship.
	LANHostname string `json:"lan_hostname,omitempty"` // <sub>-lan.<publicHost>
	LANCertPEM  string `json:"lan_cert_pem,omitempty"` // full chain
	LANKeyPEM   string `json:"lan_key_pem,omitempty"`  // private key
}

func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func ReadJSONLine(r *bufio.Reader, v any) error {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(line) == 0 {
		return errors.New("empty line")
	}
	return json.Unmarshal(line, v)
}
