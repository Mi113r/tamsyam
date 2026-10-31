// Package client connects to a tamsyam server and forwards tunnel traffic to a local target.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"

	"github.com/Mi113r/tamsyam/internal/protocol"
)

type Options struct {
	Server    string // e.g. https://t.example.com
	Token     string
	Proto     string // "http" (default) or "tcp"
	Subdomain string // HTTP: empty lets the server pick one
	Port      string // TCP: public port to reserve; empty lets the server pick one
	Allow     string // comma-separated IPs/CIDRs allowed to use the tunnel
	Target    string // host:port of the local service

	// OnConnect is called with the public URL after every (re)connect.
	OnConnect func(url string)
	// OnDisconnect is called when the session drops and a reconnect is scheduled.
	OnDisconnect func(err error, retryIn time.Duration)
	// OnEvent is called for every request (HTTP) or finished connection (TCP).
	OnEvent func(protocol.Event)
}

// FatalError is a server rejection that reconnecting will not fix.
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// Run keeps the tunnel up until ctx is cancelled or the server rejects the client.
func Run(ctx context.Context, opts Options) error {
	backoff := time.Second
	reconnect := false
	for {
		connectedAt := time.Now()
		url, sess, err := dial(ctx, opts, reconnect)
		if err == nil {
			// Keep the same address across reconnects.
			if opts.Subdomain == "" && opts.Proto != "tcp" {
				opts.Subdomain = subdomainOf(url)
			}
			if opts.Port == "" && opts.Proto == "tcp" {
				opts.Port, reconnect = portOf(url), true
			}
			if opts.OnConnect != nil {
				opts.OnConnect(url)
			}
			err = serve(ctx, sess, opts)
		}
		if ctx.Err() != nil {
			return nil
		}
		var fatal *FatalError
		if errors.As(err, &fatal) {
			return err
		}
		if time.Since(connectedAt) > time.Minute {
			backoff = time.Second
		}
		if opts.OnDisconnect != nil {
			opts.OnDisconnect(err, backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func dial(ctx context.Context, opts Options, reconnect bool) (string, *yamux.Session, error) {
	wsURL := strings.TrimSuffix(opts.Server, "/") + protocol.ConnectPath
	wsURL = strings.Replace(strings.Replace(wsURL, "https://", "wss://", 1), "http://", "ws://", 1)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+opts.Token)
	hdr.Set(protocol.HeaderTarget, opts.Target)
	hdr.Set(protocol.HeaderVersion, protocol.Version)
	for h, v := range map[string]string{
		protocol.HeaderProto:     opts.Proto,
		protocol.HeaderSubdomain: opts.Subdomain,
		protocol.HeaderPort:      opts.Port,
		protocol.HeaderAllow:     opts.Allow,
	} {
		if v != "" {
			hdr.Set(h, v)
		}
	}
	if reconnect {
		hdr.Set(protocol.HeaderReconnect, "1")
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader:      hdr,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			return "", nil, rejection(resp)
		}
		return "", nil, err
	}

	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	sess, err := yamux.Server(conn, protocol.YamuxConfig())
	if err != nil {
		conn.Close()
		return "", nil, err
	}
	return resp.Header.Get(protocol.HeaderURL), sess, nil
}

func rejection(resp *http.Response) error {
	body := ""
	if resp.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		body = strings.TrimSpace(string(b))
	}
	if body == "" {
		body = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusConflict:
		return &FatalError{Msg: body}
	}
	return fmt.Errorf("server: %s", body)
}

func serve(ctx context.Context, sess *yamux.Session, opts Options) error {
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-sess.CloseChan():
		}
	}()
	// The control stream is the only one the client opens; the server streams events on it.
	if ctl, err := sess.Open(); err == nil {
		go readEvents(ctl, opts.OnEvent)
	}
	isHTTP := opts.Proto != "tcp"
	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go forward(stream, opts.Target, isHTTP)
	}
}

func readEvents(ctl net.Conn, onEvent func(protocol.Event)) {
	defer ctl.Close()
	dec := json.NewDecoder(ctl)
	for {
		var ev protocol.Event
		if err := dec.Decode(&ev); err != nil {
			return
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
}

func forward(stream net.Conn, target string, isHTTP bool) {
	defer stream.Close()
	local, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		if isHTTP {
			msg := "tamsyam: cannot reach " + target + ": " + err.Error() + "\n"
			fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		}
		return
	}
	defer local.Close()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(local, stream); closeWrite(local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(stream, local); done <- struct{}{} }()
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

func subdomainOf(url string) string {
	_, host, ok := strings.Cut(url, "://")
	if !ok {
		return ""
	}
	sub, _, _ := strings.Cut(host, ".")
	return sub
}

func portOf(url string) string {
	if i := strings.LastIndex(url, ":"); i >= 0 && !strings.HasPrefix(url[i:], "://") {
		return url[i+1:]
	}
	return ""
}
