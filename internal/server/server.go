// Package server implements the tunnel server: it accepts client connections,
// routes public HTTP traffic by subdomain, and serves the admin panel.
package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/Mi113r/tamsyam/internal/protocol"
	"github.com/Mi113r/tamsyam/internal/store"
)

type Config struct {
	// Domain is the base host. The admin panel and client endpoint live on it,
	// tunnels on <sub>.<Domain>.
	Domain string
	// PublicScheme is used to build tunnel URLs ("https" behind Caddy).
	PublicScheme string
	// PublicPort is appended to tunnel URLs when non-empty (local testing).
	PublicPort    string
	AdminPassword string

	// TCPPortMin..TCPPortMax is the public port range for TCP tunnels; 0 disables them.
	TCPPortMin, TCPPortMax int
	// TCPHost is the host shown in tcp:// addresses (defaults to Domain).
	TCPHost string
	// TCPBind is the address TCP listeners bind to (defaults to all interfaces).
	TCPBind string

	// PrivateNetworks are CIDRs (or plain IPs) whose clients may use the admin
	// panel without the password (trusted LAN). Everyone else — including VPN
	// clients — still needs it; an empty list means password everywhere.
	PrivateNetworks []netip.Prefix
	// TrustedProxies are the fronting proxies (Caddy) whose X-Forwarded-For is
	// believed. Loopback is always trusted; a proxy on another host must be listed,
	// otherwise every request appears to come from the proxy itself.
	TrustedProxies []netip.Prefix

	// AuthMaxFailures failed logins (admin password or client token) within
	// AuthWindow block the client IP until the window ends. Defaults: 10, 15 min.
	AuthMaxFailures int
	AuthWindow      time.Duration
}

// ParseNetworks parses a comma-separated list of CIDRs and plain IPs
// (TAMSYAM_PRIVATE_NETWORKS, TAMSYAM_TRUSTED_PROXIES).
func ParseNetworks(list string) ([]netip.Prefix, error) {
	nets, _, err := parseAllow(list)
	return nets, err
}

type Server struct {
	cfg      Config
	store    *store.Store
	registry *Registry
	stats    *Collector
	log      *slog.Logger
	admin    http.Handler
	limiter  *authLimiter

	warnProxy sync.Once
}

// New creates the server and starts its statistics collector; call Close to
// stop it and flush pending counters.
func New(cfg Config, st *store.Store, log *slog.Logger) *Server {
	cfg.Domain = strings.ToLower(strings.TrimSuffix(cfg.Domain, "."))
	if cfg.TCPHost == "" {
		cfg.TCPHost = cfg.Domain
	}
	if cfg.AuthMaxFailures <= 0 {
		cfg.AuthMaxFailures = 10
	}
	if cfg.AuthWindow <= 0 {
		cfg.AuthWindow = 15 * time.Minute
	}
	s := &Server{cfg: cfg, store: st, registry: NewRegistry(), stats: newCollector(st, log), log: log,
		limiter: newAuthLimiter(cfg.AuthMaxFailures, cfg.AuthWindow)}
	s.admin = s.adminRoutes()
	go s.stats.run()
	return s
}

func (s *Server) Close() { s.stats.close() }

// FlushStats writes in-memory statistics to the store.
func (s *Server) FlushStats() error { return s.stats.Flush() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.resolveClient(r) {
		r = r.WithContext(context.WithValue(r.Context(), unknownProxyKey{}, true))
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	if host == s.cfg.Domain {
		if r.URL.Path == protocol.ConnectPath {
			s.handleConnect(w, r)
			return
		}
		s.admin.ServeHTTP(w, r)
		return
	}

	sub, ok := strings.CutSuffix(host, "."+s.cfg.Domain)
	if !ok || strings.Contains(sub, ".") {
		renderError(w, http.StatusNotFound, "Unknown host", "This server does not serve "+host+".")
		return
	}
	t := s.registry.Get(sub)
	if t == nil {
		renderError(w, http.StatusNotFound, "Tunnel not found",
			"No client is connected for "+host+". Start it with: ts http <port> -s "+sub)
		return
	}
	t.ServeHTTP(w, r)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.limiter.blocked(ip); wait > 0 {
		tooManyAttempts(w, wait)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	tok, err := s.store.Authenticate(token)
	if errors.Is(err, store.ErrNotFound) {
		s.authFailed(ip, "client token")
		http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.log.Error("authenticate", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.limiter.succeed(ip)

	allow, allowText, err := parseAllow(r.Header.Get(protocol.HeaderAllow))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target := r.Header.Get(protocol.HeaderTarget)
	if target == "" {
		target = "localhost"
	}

	var (
		proto = strings.ToLower(r.Header.Get(protocol.HeaderProto))
		key   string
		sub   string
		port  int
		url   string
		ln    net.Listener
	)
	switch proto {
	case "", "http":
		proto = "http"
		sub = strings.ToLower(r.Header.Get(protocol.HeaderSubdomain))
		if sub == "" {
			for sub = randomSubdomain(); s.registry.Get(sub) != nil; sub = randomSubdomain() {
			}
		} else if !validSubdomain(sub) {
			http.Error(w, errSubdomainInvalid.Error(), http.StatusBadRequest)
			return
		} else if s.registry.Taken(sub, tok.ID) {
			http.Error(w, errSubdomainTaken.Error(), http.StatusConflict)
			return
		}
		key, url = sub, s.tunnelURL(sub)
	case "tcp":
		ln, port, err = s.listenTCP(tok.ID, r.Header.Get(protocol.HeaderPort), r.Header.Get(protocol.HeaderReconnect) == "")
		switch {
		case errors.Is(err, errTCPDisabled), errors.Is(err, errPortRange):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		key, url = tcpKey(port), fmt.Sprintf("tcp://%s:%d", s.cfg.TCPHost, port)
	default:
		http.Error(w, "unknown protocol "+proto, http.StatusBadRequest)
		return
	}
	w.Header().Set(protocol.HeaderURL, url)

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.log.Warn("websocket accept", "err", err)
		if ln != nil {
			ln.Close()
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)

	sess, err := yamux.Client(conn, protocol.YamuxConfig())
	if err != nil {
		conn.Close()
		if ln != nil {
			ln.Close()
		}
		return
	}
	t := newTunnel(sess, s.stats)
	t.Key, t.Proto, t.Subdomain, t.Port, t.URL, t.Target = key, proto, sub, port, url, target
	t.allow, t.Allow = allow, allowText
	t.TokenID, t.TokenName = tok.ID, tok.Name
	t.ClientAddr, t.ClientVer = clientIP(r), r.Header.Get(protocol.HeaderVersion)
	t.listener = ln
	if proto == "http" {
		t.enableHTTP()
	}

	if err := s.registry.Add(t); err != nil {
		t.Close()
		return
	}
	if proto == "tcp" {
		go t.serveTCP()
	}
	go t.runControl()
	s.log.Info("tunnel up", "key", key, "token", tok.Name, "client", t.ClientAddr, "target", target)

	<-sess.CloseChan()
	t.Close()
	s.registry.Remove(t)
	s.log.Info("tunnel down", "key", key, "token", tok.Name)
}

// authFailed counts a failed login and logs when it gets the client blocked.
func (s *Server) authFailed(ip, what string) {
	if s.limiter.fail(ip) {
		s.log.Warn("too many failed logins, client blocked", "ip", ip, "auth", what, "for", s.cfg.AuthWindow)
	}
}

func (s *Server) tunnelURL(sub string) string {
	host := sub + "." + s.cfg.Domain
	if s.cfg.PublicPort != "" {
		host += ":" + s.cfg.PublicPort
	}
	return s.cfg.PublicScheme + "://" + host
}

type unknownProxyKey struct{}

// resolveClient replaces r.RemoteAddr with the original client address.
// X-Forwarded-For is honored only when the direct peer is a trusted proxy, and
// it is read from the right: the first hop that is not a trusted proxy is the
// client, so values a client prepends itself are never used. It reports
// whether r carries X-Forwarded-For from an untrusted peer; the address then
// stays the peer's, which may be a proxy rather than the client.
func (s *Server) resolveClient(r *http.Request) (unknownProxy bool) {
	xff := r.Header.Values("X-Forwarded-For")
	if len(xff) == 0 {
		return false
	}
	if peer := peerAddr(r.RemoteAddr); !s.trustedProxy(peer) {
		s.warnProxy.Do(func() {
			s.log.Warn("ignoring X-Forwarded-For from an untrusted peer; if it is your proxy, add it to TAMSYAM_TRUSTED_PROXIES", "peer", peer)
		})
		return true
	}
	hops := strings.Split(strings.Join(xff, ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		ip = ip.Unmap()
		r.RemoteAddr = netip.AddrPortFrom(ip, 0).String()
		if !s.trustedProxy(ip) {
			break
		}
	}
	return false
}

func (s *Server) trustedProxy(ip netip.Addr) bool {
	return ip.IsLoopback() || containsAddr(s.cfg.TrustedProxies, ip)
}

func peerAddr(remoteAddr string) netip.Addr {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

func containsAddr(nets []netip.Prefix, ip netip.Addr) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the client address; ServeHTTP has already resolved it
// through trusted proxies.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

var errorPage = template.Must(template.New("err").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · tamsyam</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;font:16px/1.5 system-ui,sans-serif;background:#f6f7f9;color:#1d2330}
@media (prefers-color-scheme:dark){body{background:#14161b;color:#e6e8ee}.c{background:#1c1f26!important;border-color:#2a2e38!important}}
.c{max-width:520px;margin:16px;padding:32px;background:#fff;border:1px solid #e3e6ec;border-radius:12px}
h1{margin:0 0 8px;font-size:20px}p{margin:0;opacity:.75}code{font-size:14px}.s{font-size:13px;opacity:.5;margin-bottom:12px}
</style></head><body><div class="c"><div class="s">{{.Code}}</div><h1>{{.Title}}</h1><p>{{.Text}}</p></div></body></html>`))

func renderError(w http.ResponseWriter, code int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = errorPage.Execute(w, map[string]any{"Code": fmt.Sprintf("%d %s", code, http.StatusText(code)), "Title": title, "Text": text})
}
