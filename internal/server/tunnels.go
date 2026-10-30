package server

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

var (
	errSubdomainTaken   = errors.New("subdomain is already in use")
	errSubdomainInvalid = errors.New("subdomain must be 1-63 chars of a-z, 0-9 and '-'")

	subdomainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reserved    = map[string]bool{"www": true, "admin": true, "api": true, "mail": true, "app": true}
)

const requestLogSize = 200

// TunnelInfo is the public, serializable view of a tunnel.
type TunnelInfo struct {
	Subdomain   string    `json:"subdomain"`
	URL         string    `json:"url"`
	Target      string    `json:"target"`
	TokenID     int64     `json:"token_id"`
	TokenName   string    `json:"token_name"`
	ClientAddr  string    `json:"client_addr"`
	ClientVer   string    `json:"client_version"`
	ConnectedAt time.Time `json:"connected_at"`
	Requests    int64     `json:"requests"`
	Streams     int       `json:"streams"`
}

// Tunnel is one connected client exposed under a subdomain.
type Tunnel struct {
	TunnelInfo

	session  *yamux.Session
	proxy    *httputil.ReverseProxy
	requests atomic.Int64
	seq      atomic.Uint64

	logMu sync.Mutex
	log   []*exchange // ring buffer, newest last
}

func newTunnel(sess *yamux.Session) *Tunnel {
	t := &Tunnel{session: sess}
	t.ConnectedAt = time.Now().UTC()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return sess.Open()
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
	return t
}

func (t *Tunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) { t.serve(w, r, false) }

// serve proxies one request and records it in the inspector log, returning its ID.
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

	t.requests.Add(1)
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

func (t *Tunnel) snapshot() TunnelInfo {
	info := t.TunnelInfo
	info.Requests = t.requests.Load()
	info.Streams = t.session.NumStreams()
	return info
}

func (t *Tunnel) Close() { t.session.Close() }

// Registry tracks active tunnels by subdomain.
type Registry struct {
	mu      sync.RWMutex
	tunnels map[string]*Tunnel
}

func NewRegistry() *Registry { return &Registry{tunnels: map[string]*Tunnel{}} }

func (r *Registry) Get(sub string) *Tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[sub]
}

// Add registers t. A subdomain held by the same token is taken over, which lets a
// client reconnect before the server notices its previous session died.
func (r *Registry) Add(t *Tunnel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.tunnels[t.Subdomain]; ok {
		if old.TokenID != t.TokenID {
			return errSubdomainTaken
		}
		old.Close()
	}
	r.tunnels[t.Subdomain] = t
	return nil
}

func (r *Registry) Remove(t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tunnels[t.Subdomain] == t {
		delete(r.tunnels, t.Subdomain)
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

// Taken reports whether sub is held by a token other than tokenID.
func (r *Registry) Taken(sub string, tokenID int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tunnels[sub]
	return ok && t.TokenID != tokenID
}

func validSubdomain(s string) bool { return subdomainRe.MatchString(s) && !reserved[s] }

func randomSubdomain() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
