package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// registry maps a per-call credential to its egress allowlist. The supervisor registers a
// cred + allowlist before driving a worker call and unregisters it after (least privilege,
// D24.8) — so the worker's outbound is bound to exactly this call's allowlist.
type registry struct {
	mu     sync.RWMutex
	byCred map[string][]string
}

func newRegistry() *registry { return &registry{byCred: map[string][]string{}} }

func (r *registry) get(cred string) ([]string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byCred[cred]
	return a, ok
}
func (r *registry) set(cred string, allow []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byCred[cred] = allow
}
func (r *registry) del(cred string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byCred, cred)
}

// proxy is the forward proxy. resolve + dial are injectable for tests; in production resolve
// is net.LookupIP and dial is a bounded net.Dialer. The load-bearing property: dial connects
// to exactly the IP decide() pinned — never a re-resolved address (no rebinding TOCTOU).
type proxy struct {
	reg     *registry
	resolve func(string) ([]net.IP, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

func credOf(r *http.Request) string {
	if v, ok := strings.CutPrefix(r.Header.Get("Proxy-Authorization"), "Bearer "); ok {
		return v
	}
	return ""
}

// target extracts host+port from a proxy request (CONNECT uses r.Host; a forward HTTP request
// uses the absolute r.URL).
func target(r *http.Request) (host, port string) {
	hp := r.Host
	if r.Method != http.MethodConnect && r.URL.Host != "" {
		hp = r.URL.Host
	}
	if h, p, err := net.SplitHostPort(hp); err == nil {
		return h, p
	}
	if r.Method == http.MethodConnect {
		return hp, "443"
	}
	return hp, "80"
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cred := credOf(r)
	allow, ok := p.reg.get(cred)
	if cred == "" || !ok {
		http.Error(w, "egress: unknown or missing credential", http.StatusForbidden)
		return
	}
	host, port := target(r)
	ip, err := decide(host, port, allow, p.resolve)
	if err != nil {
		log.Printf("egress DENY %s: %v", net.JoinHostPort(host, port), err)
		http.Error(w, "egress denied", http.StatusForbidden)
		return
	}
	pinned := net.JoinHostPort(ip.String(), port)
	if r.Method == http.MethodConnect {
		p.doConnect(w, r, pinned)
		return
	}
	p.doHTTP(w, r, pinned)
}

// doConnect tunnels raw bytes to the pinned IP (HTTPS and any TCP protocol). TLS is end-to-end;
// the proxy only pipes, so it never sees plaintext.
func (p *proxy) doConnect(w http.ResponseWriter, r *http.Request, pinned string) {
	dst, err := p.dial(r.Context(), "tcp", pinned)
	if err != nil {
		http.Error(w, "egress upstream", http.StatusBadGateway)
		return
	}
	defer dst.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer src.Close()
	if _, err := src.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 1)
	go func() { io.Copy(dst, src); done <- struct{}{} }()
	io.Copy(src, dst)
	<-done
}

// doHTTP forwards a plain-HTTP request to the pinned IP with the original Host preserved. The
// one-shot transport dials only the pinned address, so the Host header cannot cause a re-resolve.
func (p *proxy) doHTTP(w http.ResponseWriter, r *http.Request, pinned string) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return p.dial(ctx, "tcp", pinned)
		},
		DisableKeepAlives: true,
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Del("Proxy-Authorization")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		http.Error(w, "egress upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// controlHandler registers/unregisters per-call allowlists. Bound to a UDS shared with the
// supervisor only (never on faasnet), so the worker cannot self-grant an allowlist.
func controlHandler(reg *registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cred  string   `json:"cred"`
			Allow []string `json:"allow"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.Cred == "" {
			http.Error(w, "bad register", http.StatusBadRequest)
			return
		}
		reg.set(body.Cred, body.Allow)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /unregister", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cred string `json:"cred"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Cred == "" {
			http.Error(w, "bad unregister", http.StatusBadRequest)
			return
		}
		reg.del(body.Cred)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	proxyAddr := env("EGRESS_PROXY_ADDR", ":8888")
	controlSock := env("EGRESS_CONTROL_SOCK", "/run/faas/egress-ctl.sock")
	dialTimeout, _ := time.ParseDuration(env("EGRESS_DIAL_TIMEOUT", "10s"))

	reg := newRegistry()
	dialer := &net.Dialer{Timeout: dialTimeout}
	p := &proxy{
		reg:     reg,
		resolve: net.LookupIP,
		dial:    dialer.DialContext,
	}

	// control listener on a UDS (supervisor only)
	_ = os.Remove(controlSock)
	cl, err := net.Listen("unix", controlSock)
	if err != nil {
		log.Fatalf("egress: control listen %s: %v", controlSock, err)
	}
	go func() {
		log.Printf("egress: control on %s", controlSock)
		log.Fatalf("egress: control server: %v", (&http.Server{Handler: controlHandler(reg)}).Serve(cl))
	}()

	srv := &http.Server{Addr: proxyAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("egress: proxy on %s", proxyAddr)
	log.Fatalf("egress: proxy server: %v", srv.ListenAndServe())
}
