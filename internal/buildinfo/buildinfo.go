// Package buildinfo carries values stamped into the binary at link time.
//
// Nothing here is a secret — these are deployment defaults so that a fleet
// can run `mygrok http 3000 --subdomain=foo` without every machine needing
// a config file. A stock `go build` leaves DefaultServer empty, which is the
// correct behaviour for a generic build: the client then insists on being
// told which server to dial.
//
// Stamp them with -ldflags, e.g.:
//
//	go build -ldflags "\
//	  -X github.com/schappim/mygrok/internal/buildinfo.Version=v1.0.0 \
//	  -X github.com/schappim/mygrok/internal/buildinfo.DefaultServer=tunnel.example.com:7000" \
//	  ./cmd/mygrok
//
// build.sh does exactly this, which is why the clients `mygrokd` serves from
// /dl already know how to find the server that served them.
package buildinfo

// Version is the release identifier reported by `mygrok version`.
var Version = "dev"

// DefaultServer is the tunnel control endpoint (host:port) used when no
// --server flag, MYGROK_SERVER env var, or config file says otherwise.
// Empty in a stock build.
var DefaultServer = ""
