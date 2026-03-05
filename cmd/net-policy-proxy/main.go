package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"
)

type rule struct {
	Host     string `json:"host"`
	CIDR     string `json:"cidr"`
	Protocol string `json:"protocol"`
	Ports    []int  `json:"ports"`
}

type allowRule struct {
	exactHost    string
	wildcardBase string
	prefix       netip.Prefix
	hasPrefix    bool
	ports        map[int]struct{}
}

type allowlist struct {
	hostRules []allowRule
	cidrRules []allowRule
}

func loadAllowlist(path string) (*allowlist, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules []rule
	if err := json.Unmarshal(b, &rules); err != nil {
		return nil, err
	}
	al := &allowlist{}
	for _, r := range rules {
		proto := strings.ToLower(strings.TrimSpace(r.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		if proto != "tcp" {
			return nil, fmt.Errorf("invalid protocol: %q", r.Protocol)
		}
		ports := r.Ports
		if len(ports) == 0 {
			ports = []int{443}
		}
		portSet := make(map[int]struct{}, len(ports))
		for _, p := range ports {
			if p <= 0 || p > 65535 {
				return nil, fmt.Errorf("invalid port: %d", p)
			}
			portSet[p] = struct{}{}
		}

		host := strings.ToLower(strings.TrimSpace(r.Host))
		cidr := strings.TrimSpace(r.CIDR)
		if host == "" && cidr == "" {
			return nil, fmt.Errorf("allowlist rule requires host or cidr")
		}
		if host != "" && cidr != "" {
			return nil, fmt.Errorf("allowlist rule cannot set both host and cidr")
		}
		ar := allowRule{ports: portSet}
		if host != "" {
			if strings.HasPrefix(host, "*.") {
				ar.wildcardBase = strings.TrimPrefix(host, "*.")
			} else {
				ar.exactHost = host
			}
			al.hostRules = append(al.hostRules, ar)
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid cidr %q: %w", cidr, err)
		}
		if !prefix.Addr().Is4() {
			continue
		}
		ar.prefix = prefix
		ar.hasPrefix = true
		al.cidrRules = append(al.cidrRules, ar)
	}
	return al, nil
}

func (a *allowlist) allowed(ctx context.Context, host string, port int) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || port <= 0 || port > 65535 {
		return false
	}
	if ip, err := netip.ParseAddr(h); err == nil && ip.Is4() {
		return a.allowedIP(ip, port)
	}
	for _, r := range a.hostRules {
		if !portAllowed(r.ports, port) {
			continue
		}
		if r.exactHost != "" && h == r.exactHost {
			return true
		}
		if r.wildcardBase != "" && (h == r.wildcardBase || strings.HasSuffix(h, "."+r.wildcardBase)) {
			return true
		}
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	if err != nil {
		return false
	}
	for _, a2 := range addrs {
		ip, ok := netip.AddrFromSlice(a2.IP)
		if !ok || !ip.Is4() {
			continue
		}
		if a.allowedIP(ip, port) {
			return true
		}
	}
	return false
}

func (a *allowlist) allowedIP(ip netip.Addr, port int) bool {
	for _, r := range a.cidrRules {
		if !r.hasPrefix {
			continue
		}
		if !portAllowed(r.ports, port) {
			continue
		}
		if r.prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func portAllowed(set map[int]struct{}, port int) bool {
	_, ok := set[port]
	return ok
}

type proxy struct {
	allow *allowlist
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, portStr, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad connect host", http.StatusBadRequest)
		return
	}
	port, err := parsePort(portStr)
	if err != nil {
		http.Error(w, "bad connect port", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !p.allow.allowed(ctx, host, port) {
		w.Header().Set("X-Nano-Policy", "blocked")
		http.Error(w, "blocked by allowlist", http.StatusForbidden)
		return
	}

	dst, err := net.DialTimeout("tcp", net.JoinHostPort(host, portStr), 10*time.Second)
	if err != nil {
		http.Error(w, "dial failed", http.StatusBadGateway)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = dst.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}

	_, _ = bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = bufrw.Flush()

	go func() {
		_, _ = io.Copy(dst, clientConn)
		_ = dst.Close()
	}()
	_, _ = io.Copy(clientConn, dst)
	_ = clientConn.Close()
}

func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL
	if !targetURL.IsAbs() {
		scheme := "http"
		u := *r.URL
		u.Scheme = scheme
		u.Host = r.Host
		targetURL = &u
	}
	host, port := splitHostPortDefault(targetURL.Host, targetURL.Scheme)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !p.allow.allowed(ctx, host, port) {
		w.Header().Set("X-Nano-Policy", "blocked")
		http.Error(w, "blocked by allowlist", http.StatusForbidden)
		return
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.URL = targetURL
	outReq.Host = targetURL.Host
	outReq.Header.Del("Proxy-Connection")

	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	resp, err := tr.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func splitHostPortDefault(hostport string, scheme string) (string, int) {
	h := hostport
	if strings.Contains(hostport, ":") {
		host, portStr, err := net.SplitHostPort(hostport)
		if err == nil {
			p, _ := parsePort(portStr)
			return host, p
		}
	}
	if scheme == "https" {
		return h, 443
	}
	return h, 80
}

func parsePort(s string) (int, error) {
	var p int
	_, err := fmt.Sscanf(s, "%d", &p)
	if err != nil || p <= 0 || p > 65535 {
		return 0, fmt.Errorf("invalid port")
	}
	return p, nil
}

func main() {
	listen := flag.String("listen", ":3128", "")
	allowPath := flag.String("allowlist", "/etc/nano/allowlist.json", "")
	flag.Parse()

	al, err := loadAllowlist(*allowPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
	s := &http.Server{
		Addr:              *listen,
		Handler:           &proxy{allow: al},
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
