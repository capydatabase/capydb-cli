// Package source inspects an import source connection string on the caller's
// machine, before anything is sent to the control plane: where the host lives
// (CapyDB cannot reach a laptop or a private network), which Postgres major it
// runs compared with the local pg_dump, and - for Supabase - which pooler host
// the project actually uses.
package source

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Endpoint is the part of a connection string that decides reachability.
type Endpoint struct {
	// Hosts lists every host in the string (libpq allows a comma-separated
	// list). A value starting with "/" is a unix-socket directory.
	Hosts []string
	Port  string
	User  string
	// IsURL reports whether the input was a postgres:// URL rather than a
	// key=value connection string, so callers know whether it can be redacted
	// with net/url.
	IsURL bool
}

// ParseEndpoint extracts hosts, port, and user from a libpq connection string
// in either URL (postgres://, postgresql://) or key=value form. It reads only
// the string: no environment variables, no service files, no network. An
// omitted host is returned as an empty Hosts list, which libpq resolves to a
// local unix socket.
func ParseEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("connection string is empty")
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return Endpoint{}, fmt.Errorf("parse connection URL: %w", err)
		}
		endpoint := Endpoint{IsURL: true, User: parsed.User.Username()}
		// url.Parse keeps "h1:5432,h2:5433" as one Host; split it the way libpq does.
		for part := range strings.SplitSeq(parsed.Host, ",") {
			host, port := splitHostPort(part)
			if host != "" {
				endpoint.Hosts = append(endpoint.Hosts, host)
			}
			if endpoint.Port == "" {
				endpoint.Port = port
			}
		}
		query := parsed.Query()
		if value := query.Get("host"); value != "" {
			endpoint.Hosts = splitList(value)
		}
		if value := query.Get("port"); value != "" {
			endpoint.Port = value
		}
		if value := query.Get("user"); value != "" {
			endpoint.User = value
		}
		return endpoint, nil
	}

	endpoint := Endpoint{}
	for _, field := range strings.Fields(raw) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return Endpoint{}, fmt.Errorf("connection string is neither a postgres:// URL nor key=value pairs")
		}
		value = strings.Trim(value, `'"`)
		switch strings.ToLower(key) {
		case "host", "hostaddr":
			endpoint.Hosts = splitList(value)
		case "port":
			endpoint.Port = value
		case "user":
			endpoint.User = value
		}
	}
	return endpoint, nil
}

func splitHostPort(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]"), port
	}
	return strings.Trim(value, "[]"), ""
}

func splitList(value string) []string {
	var out []string
	for part := range strings.SplitSeq(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Lookup resolves a hostname; net.LookupIP in production, a fake in tests.
type Lookup func(host string) ([]net.IP, error)

// PrivateHost returns the first host CapyDB could not reach from its own
// network and why, or ok=false when every host is (as far as this machine
// can tell) public.
//
// The rules mirror the control plane's SSRF gate (backend
// service.ensureImportHostAllowedWith / IsNonPublicIP) - KEEP IN LOCKSTEP - so
// the CLI stops exactly the sources the API would reject, but with an answer
// instead of a 400. A hostname this machine cannot resolve is not flagged: the
// control plane resolves it independently and gives its own verdict.
func PrivateHost(endpoint Endpoint, lookup Lookup) (host, reason string, ok bool) {
	if len(endpoint.Hosts) == 0 {
		return "", "the connection string names no host, so it connects through a local unix socket", true
	}
	for _, host := range endpoint.Hosts {
		if reason := privateReason(host, lookup); reason != "" {
			return host, reason, true
		}
	}
	return "", "", false
}

func privateReason(host string, lookup Lookup) string {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.HasPrefix(host, "/") {
		return "it is a unix-socket directory on this machine"
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipReason(ip)
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case lower == "localhost" || strings.HasSuffix(lower, ".localhost"):
		return "it is this machine (loopback)"
	case strings.HasSuffix(lower, ".internal"):
		return "it is an internal hostname"
	case strings.HasSuffix(lower, ".local"):
		return "it is a local-network (mDNS) hostname"
	}
	if lookup == nil {
		return ""
	}
	addresses, err := lookup(host)
	if err != nil {
		return ""
	}
	for _, address := range addresses {
		if reason := ipReason(address); reason != "" {
			return fmt.Sprintf("it resolves to %s, and %s", address, reason)
		}
	}
	return ""
}

// ipReason names why an address is not reachable from CapyDB, or "".
func ipReason(ip net.IP) string {
	if mapped := ip.To4(); mapped != nil && !ip.Equal(mapped) {
		ip = mapped
	}
	switch {
	case ip.IsLoopback():
		return "it is this machine (loopback)"
	case ip.IsUnspecified():
		return "it is the unspecified address"
	case ip.Equal(net.IPv4(169, 254, 169, 254)):
		return "it is a cloud metadata address"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "it is a link-local address"
	case ip.IsMulticast():
		return "it is a multicast address"
	case ip.IsPrivate():
		return "it is a private-network address (RFC 1918 / ULA)"
	}
	// 100.64.0.0/10: carrier-grade NAT and Tailscale. net.IP.IsPrivate
	// excludes it, but nothing outside that network can reach it.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return "it is a shared-address-space (CGNAT / Tailscale) address"
	}
	return ""
}

// Redacted returns the connection string safe to print: a URL with its
// password masked, or a fixed placeholder for key=value strings (which can
// carry password= anywhere).
func Redacted(raw string, endpoint Endpoint) string {
	if endpoint.IsURL {
		if parsed, err := url.Parse(strings.TrimSpace(raw)); err == nil {
			return parsed.Redacted()
		}
	}
	return "<your source connection string>"
}
