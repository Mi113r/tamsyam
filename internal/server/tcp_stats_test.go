package server_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Mi113r/tamsyam/internal/client"
	"github.com/Mi113r/tamsyam/internal/protocol"
	"github.com/Mi113r/tamsyam/internal/server"
)

// freePort returns a TCP port that is currently free on 127.0.0.1.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoServer echoes every line back and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

func tcpEnv(t *testing.T) (*env, int) {
	port := freePort(t)
	return newEnv(t, server.Config{TCPPortMin: port, TCPPortMax: port, TCPBind: "127.0.0.1"}), port
}

func (e *env) adminJSON(t *testing.T, method, path string, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
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

func (e *env) waitEvent(t *testing.T, typ string) protocol.Event {
	t.Helper()
	for {
		select {
		case ev := <-e.events:
			if ev.Type == typ {
				return ev
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %q event", typ)
		}
	}
}

func TestTCPTunnel(t *testing.T) {
	e, port := tcpEnv(t)
	url, _ := e.connect(t, client.Options{Proto: "tcp", Target: echoServer(t)})
	if want := fmt.Sprintf("tcp://%s:%d", domain, port); url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}

	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(c, "ping\n")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo: %q, %v", line, err)
	}
	c.Close()

	ev := e.waitEvent(t, "conn")
	if ev.BytesIn != 5 || ev.BytesOut != 5 || ev.RemoteIP != "127.0.0.1" {
		t.Fatalf("conn event: %+v", ev)
	}

	var conns []server.ConnEntry
	e.adminJSON(t, "GET", fmt.Sprintf("/api/tunnels/tcp:%d/connections", port), "", &conns)
	if len(conns) != 1 || conns[0].BytesIn != 5 {
		t.Fatalf("connections: %+v", conns)
	}
	var tunnels []server.TunnelInfo
	e.adminJSON(t, "GET", "/api/tunnels", "", &tunnels)
	if len(tunnels) != 1 || tunnels[0].Proto != "tcp" || tunnels[0].Stats.TotalConns != 1 || tunnels[0].Stats.BytesOut != 5 {
		t.Fatalf("tunnels: %+v", tunnels)
	}
	// A random port must not be reserved.
	var res []map[string]any
	e.adminJSON(t, "GET", "/api/reservations", "", &res)
	if len(res) != 0 {
		t.Fatalf("reservations: %+v", res)
	}
}

func TestTCPPortReservation(t *testing.T) {
	e, port := tcpEnv(t)
	target := echoServer(t)
	_, stop := e.connect(t, client.Options{Proto: "tcp", Port: fmt.Sprint(port), Target: target})
	stop()
	for range 50 { // wait for the server to drop the tunnel
		var tunnels []server.TunnelInfo
		if e.adminJSON(t, "GET", "/api/tunnels", "", &tunnels); len(tunnels) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var created struct{ Secret string }
	e.adminJSON(t, "POST", "/api/tokens", `{"name":"other"}`, &created)
	err := client.Run(t.Context(), client.Options{
		Server: e.ts.URL, Token: created.Secret, Proto: "tcp", Port: fmt.Sprint(port), Target: target,
	})
	var fatal *client.FatalError
	if !errors.As(err, &fatal) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("other token got %v", err)
	}

	var res []struct {
		Port      int    `json:"port"`
		TokenName string `json:"token_name"`
	}
	e.adminJSON(t, "GET", "/api/reservations", "", &res)
	if len(res) != 1 || res[0].Port != port || res[0].TokenName != "test" {
		t.Fatalf("reservations: %+v", res)
	}
	if code := e.adminJSON(t, "DELETE", fmt.Sprintf("/api/reservations/%d", port), "", nil); code != http.StatusNoContent {
		t.Fatalf("release: %d", code)
	}
	e.connect(t, client.Options{Token: created.Secret, Proto: "tcp", Port: fmt.Sprint(port), Target: target})
}

func TestTCPDisabled(t *testing.T) {
	e := newEnv(t, server.Config{})
	err := client.Run(t.Context(), client.Options{Server: e.ts.URL, Token: e.secret, Proto: "tcp", Target: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v", err)
	}
}

func TestAllowList(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })

	denied := setup(t, ok, func(o *client.Options) { o.Allow = "10.0.0.0/8" })
	if code, _ := denied.get(t, "demo."+domain, "/"); code != http.StatusForbidden {
		t.Fatalf("outside allow list: %d", code)
	}
	allowed := setup(t, ok, func(o *client.Options) { o.Allow = "10.0.0.0/8, 127.0.0.1" })
	if code, _ := allowed.get(t, "demo."+domain, "/"); code != http.StatusOK {
		t.Fatalf("inside allow list: %d", code)
	}

	e := newEnv(t, server.Config{})
	err := client.Run(t.Context(), client.Options{Server: e.ts.URL, Token: e.secret, Allow: "not-an-ip", Target: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "invalid IP") {
		t.Fatalf("bad allow list: %v", err)
	}
}

func TestStatsAndEvents(t *testing.T) {
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "hello")
	}))
	for range 3 {
		e.get(t, "demo."+domain, "/a?x=1")
	}
	e.get(t, "demo."+domain, "/missing")

	for range 4 {
		ev := e.waitEvent(t, "request")
		if ev.Method != "GET" || ev.Status == 0 {
			t.Fatalf("event: %+v", ev)
		}
	}

	var rep server.StatsReport
	e.adminJSON(t, "GET", "/api/stats?range=1h", "", &rep)
	tot := rep.Totals
	if tot.Requests != 4 || tot.S2xx != 3 || tot.S4xx != 1 || tot.BytesOut == 0 {
		t.Fatalf("totals: %+v", tot)
	}
	if len(rep.TopPaths) == 0 || rep.TopPaths[0].Value != "/a" || rep.TopPaths[0].Hits != 3 {
		t.Fatalf("top paths: %+v", rep.TopPaths)
	}
	if rep.UniqueIPs != 1 || len(rep.Tunnels) != 1 || rep.Tunnels[0] != "demo" || rep.BucketMinutes != 1 {
		t.Fatalf("report: %+v", rep)
	}

	// Filtering by an unknown tunnel returns nothing.
	var other server.StatsReport
	e.adminJSON(t, "GET", "/api/stats?range=24h&tunnel=nope", "", &other)
	if other.Totals.Requests != 0 || other.Range != "24h" {
		t.Fatalf("filtered: %+v", other)
	}

	var tunnels []server.TunnelInfo
	e.adminJSON(t, "GET", "/api/tunnels", "", &tunnels)
	if s := tunnels[0].Stats; s.Requests != 4 || s.S4xx != 1 || s.RPM != 4 {
		t.Fatalf("live stats: %+v", s)
	}
}

func TestSubdomainReservation(t *testing.T) {
	e := newEnv(t, server.Config{})
	_, stop := e.connect(t, client.Options{Subdomain: "mine", Target: "127.0.0.1:1"})
	stop()
	// A random subdomain is not pinned, even across reconnects.
	_, stop = e.connect(t, client.Options{Target: "127.0.0.1:1"})
	stop()

	var created struct{ Secret string }
	e.adminJSON(t, "POST", "/api/tokens", `{"name":"other"}`, &created)
	err := client.Run(t.Context(), client.Options{Server: e.ts.URL, Token: created.Secret, Subdomain: "mine", Target: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("other token got %v", err)
	}

	var res []struct {
		Kind      string `json:"kind"`
		Subdomain string `json:"subdomain"`
		TokenName string `json:"token_name"`
	}
	e.adminJSON(t, "GET", "/api/reservations", "", &res)
	if len(res) != 1 || res[0].Kind != "http" || res[0].Subdomain != "mine" || res[0].TokenName != "test" {
		t.Fatalf("reservations: %+v", res)
	}
	if code := e.adminJSON(t, "DELETE", "/api/reservations/mine", "", nil); code != http.StatusNoContent {
		t.Fatalf("release: %d", code)
	}
	e.connect(t, client.Options{Token: created.Secret, Subdomain: "mine", Target: "127.0.0.1:1"})
}

func TestTunnelBasicAuth(t *testing.T) {
	var gotAuth string
	e := setup(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "secret page")
	}), func(o *client.Options) { o.Auth = "alice:s3cret" })

	get := func(user, pass string) int {
		req, _ := http.NewRequest("GET", e.ts.URL+"/", nil)
		req.Host = "demo." + domain
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("", ""); code != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d", code)
	}
	if code := get("alice", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code := get("alice", "s3cret"); code != http.StatusOK {
		t.Fatalf("right password: %d", code)
	}
	if gotAuth != "" {
		t.Fatalf("credentials leaked to the local service: %q", gotAuth)
	}

	var tunnels []server.TunnelInfo
	e.adminJSON(t, "GET", "/api/tunnels", "", &tunnels)
	if !tunnels[0].Auth {
		t.Fatal("tunnel does not report auth")
	}

	// Basic auth makes no sense for raw TCP.
	err := client.Run(t.Context(), client.Options{Server: e.ts.URL, Token: e.secret, Proto: "tcp", Auth: "a:b", Target: "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "only available for HTTP") {
		t.Fatalf("tcp with auth: %v", err)
	}
}
