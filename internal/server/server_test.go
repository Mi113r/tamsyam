package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Mi113r/tamsyam/internal/client"
	"github.com/Mi113r/tamsyam/internal/protocol"
	"github.com/Mi113r/tamsyam/internal/server"
	"github.com/Mi113r/tamsyam/internal/store"
)

// The test server's own address doubles as the base domain, so the client can
// reach it directly and tunnel requests just override the Host header.
const domain = "127.0.0.1"

type env struct {
	ts     *httptest.Server
	srv    *server.Server
	secret string
	url    string
	events chan protocol.Event
}

// newEnv starts a server with one token ("test") and no clients.
func newEnv(t *testing.T, cfg server.Config) *env {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, secret, err := st.CreateToken("test")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Domain, cfg.PublicScheme, cfg.AdminPassword = domain, "http", "pw"
	srv := server.New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &env{ts: ts, srv: srv, secret: secret, events: make(chan protocol.Event, 100)}
}

// connect runs a client with opts (Server and Token default to the env's) and
// returns its public URL and a function that stops it.
func (e *env) connect(t *testing.T, opts client.Options) (string, context.CancelFunc) {
	t.Helper()
	if opts.Server == "" {
		opts.Server = e.ts.URL
	}
	if opts.Token == "" {
		opts.Token = e.secret
	}
	connected := make(chan string, 1)
	failed := make(chan error, 1)
	opts.OnConnect = func(u string) { connected <- u }
	opts.OnEvent = func(ev protocol.Event) { e.events <- ev }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { failed <- client.Run(ctx, opts) }()
	select {
	case u := <-connected:
		return u, cancel
	case err := <-failed:
		t.Fatalf("client failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not connect")
	}
	return "", cancel
}

func setup(t *testing.T, backend http.Handler, mods ...func(*client.Options)) *env {
	t.Helper()
	e := newEnv(t, server.Config{})
	be := httptest.NewServer(backend)
	t.Cleanup(be.Close)
	opts := client.Options{Subdomain: "demo", Target: strings.TrimPrefix(be.URL, "http://")}
	for _, m := range mods {
		m(&opts)
	}
	e.url, _ = e.connect(t, opts)
	return e
}

func (e *env) get(t *testing.T, host, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestHTTPThroughTunnel(t *testing.T) {
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello "+r.URL.RequestURI()+" fwd="+r.Header.Get("X-Forwarded-Host"))
	}))
	if e.url != "http://demo."+domain {
		t.Fatalf("url = %q", e.url)
	}
	for i := range 3 { // reuses pooled streams
		code, body := e.get(t, "demo."+domain, "/a?b=1")
		if code != 200 || body != "hello /a?b=1 fwd=demo."+domain {
			t.Fatalf("request %d: %d %q", i, code, body)
		}
	}
	if code, _ := e.get(t, "nope."+domain, "/"); code != http.StatusNotFound {
		t.Fatalf("unknown tunnel: got %d", code)
	}
}

func (e *env) admin(t *testing.T, method, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestInspectorCaptureAndReplay(t *testing.T) {
	var hits atomic.Int32
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusCreated)
		zw := gzip.NewWriter(w)
		zw.Write([]byte(`{"got":` + string(body) + `}`))
		zw.Close()
	}))

	req, _ := http.NewRequest("POST", e.ts.URL+"/items?x=1", strings.NewReader(`{"a":1}`))
	req.Host = "demo." + domain
	req.Header.Set("X-Test", "yes")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var list []server.RequestEntry
	e.admin(t, "GET", "/api/tunnels/demo/requests", &list)
	if len(list) != 1 || list[0].Method != "POST" || list[0].Path != "/items?x=1" || list[0].Status != 201 {
		t.Fatalf("list = %+v", list)
	}

	var d server.RequestDetail
	e.admin(t, "GET", fmt.Sprintf("/api/tunnels/demo/requests/%d", list[0].ID), &d)
	if d.RequestHeaders.Get("X-Test") != "yes" || d.RequestBody.Data != `{"a":1}` {
		t.Fatalf("request capture: %+v %+v", d.RequestHeaders, d.RequestBody)
	}
	if d.ResponseBody.Decoded != "gzip" || d.ResponseBody.Data != `{"got":{"a":1}}` {
		t.Fatalf("response capture: %+v", d.ResponseBody)
	}

	var replayed struct{ ID uint64 }
	if code := e.admin(t, "POST", fmt.Sprintf("/api/tunnels/demo/requests/%d/replay", d.ID), &replayed); code != 200 {
		t.Fatalf("replay: %d", code)
	}
	var rd server.RequestDetail
	e.admin(t, "GET", fmt.Sprintf("/api/tunnels/demo/requests/%d", replayed.ID), &rd)
	if !rd.Replay || rd.Status != 201 || rd.ResponseBody.Data != `{"got":{"a":1}}` || hits.Load() != 2 {
		t.Fatalf("replay detail: %+v, hits=%d", rd, hits.Load())
	}
}

func TestInspectorTruncatesLargeBodies(t *testing.T) {
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(bytes.Repeat([]byte("x"), 100_000))
	}))
	req, _ := http.NewRequest("POST", e.ts.URL+"/", bytes.NewReader(bytes.Repeat([]byte("y"), 100_000)))
	req.Host = "demo." + domain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(got) != 100_000 {
		t.Fatalf("client got %d bytes", len(got))
	}

	var list []server.RequestEntry
	e.admin(t, "GET", "/api/tunnels/demo/requests", &list)
	var d server.RequestDetail
	e.admin(t, "GET", fmt.Sprintf("/api/tunnels/demo/requests/%d", list[0].ID), &d)
	if !d.RequestBody.Truncated || d.RequestBody.Size != 100_000 || len(d.RequestBody.Data) != 32<<10 {
		t.Fatalf("request body: truncated=%v size=%d len=%d", d.RequestBody.Truncated, d.RequestBody.Size, len(d.RequestBody.Data))
	}
	if !d.ResponseBody.Truncated || d.ResponseBody.Size != 100_000 {
		t.Fatalf("response body: %+v", d.ResponseBody.Size)
	}
	if code := e.admin(t, "POST", fmt.Sprintf("/api/tunnels/demo/requests/%d/replay", d.ID), nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("replay of truncated body: %d", code)
	}
}

func TestWebSocketThroughTunnel(t *testing.T) {
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		typ, msg, err := c.Read(r.Context())
		if err != nil {
			return
		}
		c.Write(r.Context(), typ, append([]byte("echo: "), msg...))
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, strings.Replace(e.ts.URL, "http", "ws", 1)+"/ws",
		&websocket.DialOptions{Host: "demo." + domain})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err := c.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	_, msg, err := c.Read(ctx)
	if err != nil || string(msg) != "echo: ping" {
		t.Fatalf("got %q, %v", msg, err)
	}
}

func TestAdminRequiresPassword(t *testing.T) {
	e := setup(t, http.NotFoundHandler())
	if code, _ := e.get(t, domain, "/api/tunnels"); code != http.StatusUnauthorized {
		t.Fatalf("no auth: got %d", code)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/tunnels", nil)
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"subdomain":"demo"`) {
		t.Fatalf("tunnels: %d %s", resp.StatusCode, body)
	}
}

func TestAdminTrustedNetworks(t *testing.T) {
	private, err := server.ParsePrivateNetworks("10.1.1.0/24, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, server.Config{PrivateNetworks: private})

	// Loopback via a same-host proxy: X-Forwarded-For decides.
	// LAN source — no password.
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/tunnels", nil)
	req.Host = domain
	req.Header.Set("X-Forwarded-For", "10.1.1.77")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LAN without password: got %d", resp.StatusCode)
	}

	// VPN source — password still required.
	req, _ = http.NewRequest("GET", e.ts.URL+"/api/tunnels", nil)
	req.Host = domain
	req.Header.Set("X-Forwarded-For", "10.8.0.5")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("VPN without password: got %d", resp.StatusCode)
	}

	// Direct connection from a public address: X-Forwarded-For must be ignored
	// even if it claims a trusted network.
	req, _ = http.NewRequest("GET", "/api/tunnels", nil)
	req.Host = domain
	req.Header.Set("X-Forwarded-For", "10.1.1.77")
	req.RemoteAddr = "203.0.113.9:4444"
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("spoofed XFF from public addr: got %d", rec.Code)
	}
}

func TestRejectsBadToken(t *testing.T) {
	e := setup(t, http.NotFoundHandler())
	err := client.Run(context.Background(), client.Options{
		Server: e.ts.URL, Token: "tsm_wrong", Target: "127.0.0.1:1",
	})
	var fatal *client.FatalError
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v", err)
	}
}

func TestSubdomainConflict(t *testing.T) {
	e := setup(t, http.NotFoundHandler())
	// A second token on the same server must not steal "demo".
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/tokens", strings.NewReader(`{"name":"other"}`))
	req.SetBasicAuth("admin", "pw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_, rest, _ := strings.Cut(string(body), `"secret":"`)
	other, _, _ := strings.Cut(rest, `"`)

	err = client.Run(context.Background(), client.Options{
		Server: e.ts.URL, Token: other, Subdomain: "demo", Target: "127.0.0.1:1",
	})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
}
