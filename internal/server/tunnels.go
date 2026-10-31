package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Mi113r/tamsyam/internal/protocol"
)

var (
	errSubdomainTaken   = errors.New("subdomain is already in use")
	errSubdomainInvalid = errors.New("subdomain must be 1-63 chars of a-z, 0-9 and '-'")

	subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reserved    = map[string]bool{"www": true, "admin": true, "api": true, "mail": true, "app": true}
)

const (
	requestLogSize = 200
	eventBuffer    = 256
)

// TunnelInfo is the public, serializable view of a tunnel.
type TunnelInfo struct {
	// Key identifies the tunnel: the subdomain for HTTP, "tcp:<port>" for TCP.
	Key         string    `json:"key"`
	Proto       string    `json:"proto"`
	Subdomain   string    `json:"subdomain,omitempty"`
	Port        int       `json:"port,omitempty"`
	URL         string    `json:"url"`
	Target      string    `json:"target"`
	Allow       []string  `json:"allow"`
	TokenID     int64     `json:"token_id"`
	TokenName   string    `json:"token_name"`
	ClientAddr  string    `json:"client_addr"`
	ClientVer   string    `json:"client_version"`
	ConnectedAt time.Time `json:"connected_at"`
	Stats       LiveStats `json:"stats"`
	Streams     int       `json:"streams"`
}

// Tunnel is one connected client exposed under a subdomain or a TCP port.
type Tunnel struct {
	TunnelInfo

	session  *yamux.Session
	proxy    *httputil.ReverseProxy // HTTP only
	listener net.Listener           // TCP only
	allow    []netip.Prefix
	stats    *Collector
	live     liveStats
	events   chan protocol.Event
	seq      atomic.Uint64

	logMu sync.Mutex
	log   []*exchange // HTTP ring buffer, newest last
	conns []ConnEntry // TCP ring buffer, newest last
}

func newTunnel(sess *yamux.Session, stats *Collector) *Tunnel {
	t := &Tunnel{session: sess, stats: stats, events: make(chan protocol.Event, eventBuffer)}
	t.ConnectedAt = time.Now().UTC()
	return t
}

// enableHTTP sets up the reverse proxy that carries HTTP requests over the session.
func (t *Tunnel) enableHTTP() {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return t.session.Open()
		},
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	t.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = t.Target
			// Dev servers (Vite, webpack, etc.) often reject foreign Host headers.
			pr.Out.Host = t.Target
			// We sit behind a trusted reverse proxy (Caddy): keep its forwarding headers.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto"} {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					pr.Out.Header[h] = v
				}
			}
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
		},
		Transport:     transport,
		FlushInterval: -1, // stream responses (SSE, long polling) immediately
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			renderError(w, http.StatusBadGateway, "Local service unavailable",
				"The tunnel is up, but the client could not reach "+t.Target+".")
		},
	}
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.allowed(clientIP(r)) {
		renderError(w, http.StatusForbidden, "Access denied", "Your IP address is not allowed to use this tunnel.")
		return
	}
	t.serve(w, r, false)
}

// serve proxies one request, records it in the inspector log and statistics,
// and returns its ID.
func (t *Tunnel) serve(w http.ResponseWriter, r *http.Request, replay bool) uint64 {
	start := time.Now()
	ex := &exchange{
		RequestEntry: RequestEntry{
			ID:       t.seq.Add(1),
			Time:     start.UTC(),
			Method:   r.Method,
			Host:     r.Host,
			Path:     r.URL.RequestURI(),
			RemoteIP: clientIP(r),
			Replay:   replay,
			Upgrade:  r.Header.Get("Upgrade") != "",
		},
		reqHeaders: r.Header.Clone(),
	}
	rec := &statusRecorder{ResponseWriter: w}
	// Upgraded connections are hijacked; their traffic is not HTTP bodies.
	if !ex.Upgrade {
		ex.reqBody, ex.respBody = &capBuffer{}, &capBuffer{}
		rec.body = ex.respBody
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &teeBody{ReadCloser: r.Body, buf: ex.reqBody}
		}
	}

	t.proxy.ServeHTTP(rec, r)

	ex.Status, ex.respHeaders = rec.status, rec.header
	if ex.Status == 0 && ex.Upgrade {
		ex.Status = http.StatusSwitchingProtocols
	}
	if ex.respHeaders == nil {
		ex.respHeaders = w.Header().Clone()
	}
	ex.DurationMs = time.Since(start).Milliseconds()
	t.record(ex)

	_, in, _ := ex.reqBody.snapshot()
	_, out, _ := ex.respBody.snapshot()
	t.live.addRequest(start, ex.Status, ex.DurationMs, in, out)
	t.stats.record(sample{
		tunnel: t.Key, at: start, status: ex.Status, durMs: ex.DurationMs,
		bytesIn: in, bytesOut: out, remoteIP: ex.RemoteIP, path: r.URL.Path,
	})
	t.emit(protocol.Event{
		Type: "request", Time: ex.Time, RemoteIP: ex.RemoteIP, DurationMs: ex.DurationMs,
		BytesIn: in, BytesOut: out, Method: ex.Method, Path: ex.Path, Status: ex.Status,
	})
	return ex.ID
}

func (t *Tunnel) record(ex *exchange) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if len(t.log) >= requestLogSize {
		t.log[0] = nil
		t.log = t.log[1:]
	}
	t.log = append(t.log, ex)
}

func (t *Tunnel) find(id uint64) *exchange {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	for _, ex := range t.log {
		if ex.ID == id {
			return ex
		}
	}
	return nil
}

// RequestLog returns the recorded request summaries, newest first.
func (t *Tunnel) RequestLog() []RequestEntry {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	out := make([]RequestEntry, len(t.log))
	for i, ex := range t.log {
		out[len(t.log)-1-i] = ex.RequestEntry
	}
	return out
}

func (t *Tunnel) RequestDetail(id uint64) (RequestDetail, error) {
	ex := t.find(id)
	if ex == nil {
		return RequestDetail{}, errRequestNotFound
	}
	return ex.detail(), nil
}

// emit queues an event for the client; events are dropped if it falls behind.
func (t *Tunnel) emit(ev protocol.Event) {
	select {
	case t.events <- ev:
	default:
	}
}

// runControl accepts the client's control stream and streams events to it.
func (t *Tunnel) runControl() {
	stream, err := t.session.Accept()
	if err != nil {
		return
	}
	defer stream.Close()
	enc := json.NewEncoder(stream)
	for {
		select {
		case ev := <-t.events:
			if enc.Encode(ev) != nil {
				return
			}
		case <-t.session.CloseChan():
			return
		}
	}
}

func (t *Tunnel) allowed(ip string) bool {
	if len(t.allow) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return containsAddr(t.allow, addr.Unmap())
}

func (t *Tunnel) snapshot() TunnelInfo {
	info := t.TunnelInfo
	info.Stats = t.live.snapshot()
	info.Streams = t.session.NumStreams()
	return info
}

// Close disconnects the client. For TCP tunnels the port is released immediately.
func (t *Tunnel) Close() {
	if t.listener != nil {
		t.listener.Close()
	}
	t.session.Close()
}

// parseAllow parses a comma-separated list of IPs and CIDRs.
func parseAllow(s string) ([]netip.Prefix, []string, error) {
	var (
		prefixes []netip.Prefix
		text     = []string{}
	)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			addr, aerr := netip.ParseAddr(part)
			if aerr != nil {
				return nil, nil, fmt.Errorf("invalid IP or CIDR %q", part)
			}
			addr = addr.Unmap()
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, p.Masked())
		text = append(text, p.Masked().String())
	}
	return prefixes, text, nil
}

// Registry tracks active tunnels by key.
type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
}

func NewRegistry() *Registry { return &Registry{tunnels: map[string]*Tunnel{}} }

func (r *Registry) Get(key string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[key]
}

// Add registers t. A key held by the same token is taken over, which lets a
// client reconnect before the server notices its previous session died.
func (r *Registry) Add(t *Tunnel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.tunnels[t.Key]; ok {
		if old.TokenID != t.TokenID {
			return errSubdomainTaken
		}
		old.Close()
	}
	r.tunnels[t.Key] = t
	return nil
}

func (r *Registry) Remove(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tunnels[t.Key] == t {
		delete(r.tunnels, t.Key)
	}
}

func (r *Registry) List() []TunnelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TunnelInfo, 0, len(r.tunnels))
	for _, t := range r.tunnels {
		out = append(out, t.snapshot())
	}
	return out
}

// Taken reports whether key is held by a token other than tokenID.
func (r *Registry) Taken(key string, tokenID int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tunnels[key]
	return ok && t.TokenID != tokenID
}

func tcpKey(port int) string { return fmt.Sprintf("tcp:%d", port) }

func validSubdomain(s string) bool { return subdomainRe.MatchString(s) && !reserved[s] }

func randomSubdomain() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
