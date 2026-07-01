// Command faas-egress is the FaaS policy egress proxy (F24.4/D24.8): the worker container
// has NO default route; its only next-hop off faasnet is this proxy. The proxy RESOLVES the
// destination host itself and CONNECTS to that resolved IP (IP-pinned — no re-resolution, so
// no DNS-rebinding TOCTOU), validating the connect IP against the call's egress_allow plus a
// fixed deny-set (loopback/link-local/private/multicast/metadata). Because there is no other
// route to the internet, a node:net raw-socket escape in the worker still lands here and is
// denied — the wall is the network layer, not a JS control.
package main

import (
	"fmt"
	"net"
)

// denyExtra holds ranges not already covered by the net.IP.Is* helpers (CGNAT, this-network,
// benchmarking, reserved, broadcast). Loopback/link-local(incl. 169.254 metadata)/private/
// multicast/unspecified are handled by the stdlib methods in isDenied.
var denyExtra = mustCIDRs(
	"0.0.0.0/8",          // "this network"
	"100.64.0.0/10",      // CGNAT
	"192.0.0.0/24",       // IETF protocol assignments
	"198.18.0.0/15",      // benchmarking
	"240.0.0.0/4",        // reserved
	"255.255.255.255/32", // limited broadcast
	"64:ff9b::/96",       // NAT64 (can map to private v4)
	"100::/64",           // discard-only
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("faas-egress: bad deny CIDR " + c + ": " + err.Error())
		}
		out = append(out, n)
	}
	return out
}

// isDenied reports whether an IP is in the SSRF deny-set: loopback, link-local (incl. the
// cloud metadata address 169.254.169.254), private (RFC1918 / fc00::/7), multicast,
// unspecified, plus the denyExtra reserved ranges. A nil IP is denied.
func isDenied(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	for _, n := range denyExtra {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostAllowed reports whether host[:port] matches an egress_allow entry. An entry may be a
// bare host (any port) or an exact host:port.
func hostAllowed(host, port string, allow []string) bool {
	for _, a := range allow {
		if a == host || a == host+":"+port {
			return true
		}
	}
	return false
}

// decide is the load-bearing egress decision. It returns the exact IP to connect to (IP-pinned)
// or an error, given the call's allowlist and a resolver:
//
//  1. host[:port] must be in egress_allow, else DENY (T5a).
//  2. if the allowlisted host is a LITERAL IP, connect to it directly — the operator explicitly
//     opted this exact address in (e.g. a private HA), so the deny-set is bypassed for it (T5b
//     private opt-in / T5d).
//  3. otherwise the host is a NAME: resolve it and pin the first NON-denied IP. A name that
//     resolves only to deny-set IPs (rebinding to private/metadata) is DENIED (T5b/T5c). The
//     caller connects to exactly the returned IP — no re-resolution, so there is no TOCTOU
//     window between the check and the connect.
func decide(host, port string, allow []string, lookup func(string) ([]net.IP, error)) (net.IP, error) {
	if !hostAllowed(host, port, allow) {
		return nil, fmt.Errorf("host %q not in egress_allow", net.JoinHostPort(host, port))
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil // operator-allowlisted literal IP (deny-set bypassed by explicit opt-in)
	}
	ips, err := lookup(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !isDenied(ip) {
			return ip, nil // IP-pinned: connect to exactly this resolved IP
		}
	}
	return nil, fmt.Errorf("host %q resolves only to denied IPs (rebind/private/metadata)", host)
}
