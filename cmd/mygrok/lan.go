package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

// pickLANIP returns the first RFC1918 IPv4 address found on a non-
// loopback, "up" interface. Empty string if no candidate exists (e.g.,
// only a public IP or only IPv6). Used when the user passes --lan
// without an explicit value.
func pickLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip = ip.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if !ip.IsPrivate() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

// serveLAN starts a TLS reverse-proxy listener on lanIP:lanPort using
// the cert/key the server shipped in HelloResp. Each accepted
// connection is bidirectionally pumped to target (the same local
// backend the tunnel forwards to). Returns the listener's stop func
// and any setup error; the listener runs until the func is invoked.
func serveLAN(lanIP string, lanPort int, lanHostname, certPEM, keyPEM, target string) (stop func(), err error) {
	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
	addr := net.JoinHostPort(lanIP, fmt.Sprintf("%d", lanPort))
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if !strings.Contains(err.Error(), "use of closed") {
					log.Printf("lan accept: %v", err)
				}
				return
			}
			go pumpLAN(c, target)
		}
	}()
	return func() { _ = ln.Close() }, nil
}

// pumpLAN handles one accepted LAN connection: open a fresh dial to
// the local backend and full-duplex copy bytes. Mirrors the simple
// proxyToLocal path but without the per-request HTTP framing — TLS is
// already terminated, the local app sees the request as if it came
// from a same-host visitor.
func pumpLAN(c net.Conn, target string) {
	defer c.Close()
	d, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer d.Close()
	go func() {
		_, _ = io.Copy(d, c)
		if t, ok := d.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		}
	}()
	_, _ = io.Copy(c, d)
}
