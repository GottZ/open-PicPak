package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startBackend starts a loopback HTTP server and returns its host:port.
func startBackend(t *testing.T) string {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.Host)
	}))
	t.Cleanup(b.Close)
	return strings.TrimPrefix(b.URL, "http://")
}

// startProxy starts the egress proxy with a pre-registered cred->allow map, returns its addr.
func startProxy(t *testing.T, reg *registry) string {
	t.Helper()
	p := &proxy{reg: reg, resolve: net.LookupIP, dial: (&net.Dialer{}).DialContext}
	ps := httptest.NewServer(p)
	t.Cleanup(ps.Close)
	return strings.TrimPrefix(ps.URL, "http://")
}

// The backend is on loopback (deny-set), so we allowlist its LITERAL address — the operator
// opt-in path (decide step 2) — which is the only way to exercise a live tunnel to a test
// server. The deny-set + rebind logic is covered by the unit tests in policy_test.go.

func TestProxyForwardsAllowedHTTP(t *testing.T) {
	backend := startBackend(t)
	reg := newRegistry()
	reg.set("c1", []string{backend})
	proxyAddr := startProxy(t, reg)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer c1\r\nConnection: close\r\n\r\n", backend, backend)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "hello from") {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestProxyConnectTunnel(t *testing.T) {
	backend := startBackend(t)
	reg := newRegistry()
	reg.set("c1", []string{backend})
	proxyAddr := startProxy(t, reg)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer c1\r\n\r\n", backend, backend)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status %d, want 200", resp.StatusCode)
	}
	// Speak HTTP over the established tunnel.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", backend)
	resp2, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read tunneled response: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("tunneled status %d, want 200", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.HasPrefix(string(body), "hello from") {
		t.Fatalf("tunneled body %q", body)
	}
}

func TestProxyDeniesUnknownCred(t *testing.T) {
	backend := startBackend(t)
	reg := newRegistry()
	reg.set("c1", []string{backend})
	proxyAddr := startProxy(t, reg)

	if code := doGET(t, proxyAddr, backend, "Bearer WRONG"); code != http.StatusForbidden {
		t.Fatalf("unknown cred: status %d, want 403", code)
	}
	if code := doGET(t, proxyAddr, backend, ""); code != http.StatusForbidden {
		t.Fatalf("missing cred: status %d, want 403", code)
	}
}

func TestProxyDeniesUnallowlistedHost(t *testing.T) {
	backend := startBackend(t)
	reg := newRegistry()
	reg.set("c1", []string{backend}) // only the backend is allowlisted
	proxyAddr := startProxy(t, reg)

	// A different (non-allowlisted) host must be denied even with a valid cred.
	if code := doGET(t, proxyAddr, "example.invalid:80", "Bearer c1"); code != http.StatusForbidden {
		t.Fatalf("unallowlisted host: status %d, want 403", code)
	}
}

func doGET(t *testing.T, proxyAddr, hostPort, auth string) int {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	authLine := ""
	if auth != "" {
		authLine = "Proxy-Authorization: " + auth + "\r\n"
	}
	fmt.Fprintf(conn, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n%sConnection: close\r\n\r\n", hostPort, hostPort, authLine)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
