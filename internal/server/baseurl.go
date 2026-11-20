package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
)

// baseURL is the public URL of the UI (OAuth callback, Secure cookies).
func (s *Server) baseURL(r *http.Request) string {
	if v := s.knownBaseURL(r); v != "" {
		return v
	}
	return s.listenURL()
}

// knownBaseURL is --base-url, else the URL saved by the setup, else, for a
// request from loopback or a trusted proxy, the one its Host and
// X-Forwarded-Proto headers give. Anyone else cannot choose it: "".
func (s *Server) knownBaseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	if v := s.store.Get("base_url", ""); v != "" {
		return v
	}
	if !s.trustedProxy(r) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ParseTrustedProxies parses a comma-separated list of IP addresses and
// CIDR prefixes (PITLANE_TRUSTED_PROXIES).
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not an IP address or CIDR prefix", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// trustedProxy reports whether a request comes from loopback or from one of
// the configured trusted proxies.
func (s *Server) trustedProxy(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	if addr.IsLoopback() {
		return true
	}
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// listenURL derives a URL from the listen address (the machine's host name
// when it listens on every interface), used when no base URL is known.
func (s *Server) listenURL() string {
	host, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil || port == "" {
		host, port = "", "8080"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host, _ = os.Hostname()
		if host == "" {
			host = "localhost"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}
