package main

import (
	"errors"
	"net"
	"testing"
)

func TestIsDenied(t *testing.T) {
	denied := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"169.254.169.254", // cloud metadata (link-local)
		"10.0.0.5", "172.16.0.1", "172.31.255.255", "192.168.1.1", // RFC1918
		"100.64.0.1",           // CGNAT
		"0.0.0.0", "0.1.2.3",   // this-network
		"224.0.0.1", "239.1.1.1", // multicast
		"::1", "fc00::1", "fd12::1", "fe80::1", "ff02::1", "::", // v6 loopback/private/link-local/multicast/unspecified
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", // v4-mapped loopback/metadata
	}
	for _, s := range denied {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !isDenied(ip) {
			t.Errorf("isDenied(%s) = false, want true", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if isDenied(net.ParseIP(s)) {
			t.Errorf("isDenied(%s) = true, want false", s)
		}
	}
	if !isDenied(nil) {
		t.Error("isDenied(nil) = false, want true")
	}
}

func mkLookup(m map[string][]net.IP) func(string) ([]net.IP, error) {
	return func(h string) ([]net.IP, error) {
		if ips, ok := m[h]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
}

func TestDecide(t *testing.T) {
	pub := net.ParseIP("93.184.216.34")
	priv := net.ParseIP("192.168.1.50")
	meta := net.ParseIP("169.254.169.254")

	// T5a: host not in the allowlist is denied outright.
	if _, err := decide("evil.test", "443", []string{"ha.local"}, mkLookup(nil)); err == nil {
		t.Error("T5a: unallowlisted host was allowed")
	}

	// T5b: a raw metadata/private IP that is NOT allowlisted is denied (host-not-allowlisted).
	if _, err := decide("169.254.169.254", "80", []string{"ha.local"}, mkLookup(nil)); err == nil {
		t.Error("T5b: unallowlisted metadata IP was allowed")
	}
	// T5b opt-in: a private IP the operator EXPLICITLY allowlisted (literal) connects directly.
	got, err := decide("192.168.1.50", "8123", []string{"192.168.1.50:8123"}, mkLookup(nil))
	if err != nil || !got.Equal(priv) {
		t.Errorf("T5b opt-in: got %v err=%v, want %v", got, err, priv)
	}

	// T5c: an allowlisted NAME that resolves ONLY to a private/metadata IP (rebind) is denied.
	if _, err := decide("ha.local", "443", []string{"ha.local"}, mkLookup(map[string][]net.IP{"ha.local": {priv, meta}})); err == nil {
		t.Error("T5c: rebind-to-private allowlisted name was allowed")
	}

	// T5d: an allowlisted public host resolves to a public IP and is pinned to exactly that IP.
	got, err = decide("cdn.example", "443", []string{"cdn.example"}, mkLookup(map[string][]net.IP{"cdn.example": {pub}}))
	if err != nil || !got.Equal(pub) {
		t.Errorf("T5d: got %v err=%v, want %v", got, err, pub)
	}

	// IP-pin over a mixed result: skip the denied IPs, pin the first public one.
	got, err = decide("mix.example", "443", []string{"mix.example"}, mkLookup(map[string][]net.IP{"mix.example": {priv, pub}}))
	if err != nil || !got.Equal(pub) {
		t.Errorf("mixed-resolve: got %v err=%v, want the public %v", got, err, pub)
	}

	// host:port matching — a bare-host allowlist entry matches any port; a host:port entry is exact.
	if !hostAllowed("a.test", "443", []string{"a.test"}) {
		t.Error("bare-host allowlist should match any port")
	}
	if hostAllowed("a.test", "80", []string{"a.test:443"}) {
		t.Error("host:443 allowlist should not match :80")
	}
}
