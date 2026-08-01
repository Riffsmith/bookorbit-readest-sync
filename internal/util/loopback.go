package util

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// SchemeOf returns the lowercase URL scheme of rawURL, or an error if rawURL
// cannot be parsed or has no scheme. It is a pure helper used by config
// validation to make policy decisions ("is http allowed here?"); the policy
// itself lives in internal/config, keeping this layer free of any
// internal/config dependency (internal/util is the lowest package in the
// import graph).
func SchemeOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		return "", errors.New("util: URL has no scheme")
	}
	return strings.ToLower(u.Scheme), nil
}

// IsLoopbackOrLinkLocal reports whether host is a loopback or link-local
// address by its literal form only. String matches accept "localhost" and
// "localhost." (the trailing-dot form some resolvers accept). Any other
// hostname is tested as an IP via net.ParseIP; if it parses, it is checked
// against the loopback IPv4/6 and link-local IPv4/6 CIDRs. Any host that is
// not obviously private by its literal form returns false — no DNS resolution
// is performed, so a hostname like "nas.local" that happens to resolve to
// 127.0.0.1 is not auto-accepted. Callers wanting such a host over cleartext
// must opt in via config.BookOrbit.AllowInsecureTransport.
func IsLoopbackOrLinkLocal(host string) bool {
	host = strings.TrimSpace(host)
	// Unwrap an IPv6 [host] or [host]:port bracket pair first; these are the
	// only unambiguous ways to carry a port with an IPv6 literal, and the
	// bracketed host itself parses cleanly via net.ParseIP.
	if strings.HasPrefix(host, "[") {
		if idx := strings.LastIndex(host, "]"); idx != -1 {
			host = host[1:idx]
		}
	} else if c := strings.Count(host, ":"); c == 1 {
		// Non-bracketed with exactly one colon: plausibly host:port for an
		// IPv4 or hostname. Strip the :port tail when the part after the
		// colon is all digits. (Two or more colons without brackets is a
		// bare IPv6 address like ::1 or fe80::1 — never strip it, the
		// whole thing is the address.)
		if idx := strings.IndexByte(host, ':'); idx != -1 {
			tail := host[idx+1:]
			if tail != "" && strings.Trim(tail, "0123456789") == "" {
				host = host[:idx]
			}
		}
	}

	if host == "localhost" || host == "localhost." {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range loopbackOrLinkLocalNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// HostOf returns the host[:port] portion of rawURL, with no scheme or path.
// Brackets are preserved for IPv6 literals so the result can be passed back to
// IsLoopbackOrLinkLocal. It is a tiny helper used by cmd/bridge's startup
// cleartext warning; on a parse failure it returns the input verbatim so the
// warning still names something (the URL has already passed Validate by the
// time HostOf is called from main.go, so the empty/error path is defensive).
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// loopbackOrLinkLocalNets is the set of loopback and link-local CIDRs from
// Decision C. It is package-level so each IsLoopbackOrLinkLocal call does not
// rebuild it; the values are read-only after init.
var loopbackOrLinkLocalNets = mustParseCIDRs(
	"127.0.0.0/8",    // IPv4 loopback
	"169.254.0.0/16", // IPv4 link-local
	"::1/128",        // IPv6 loopback
	"fe80::/10",      // IPv6 link-local
)

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("util: invalid loopback CIDR " + cidr + ": " + err.Error())
		}
		nets = append(nets, n)
	}
	return nets
}
