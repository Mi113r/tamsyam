// Package client connects to a tamsyam server and forwards tunnel traffic to a local target.
package client

import (
	"context"
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
	Subdomain string // empty: server picks one
	Target    string // host:port of the local service

	// OnConnect is called with the public URL after every (re)connect.
	OnConnect func(url string)
	// OnDisconnect is called when the session drops and a reconnect is scheduled.
	OnDisconnect func(err error, retryIn time.Duration)
}

// FatalError is a server rejection that reconnecting will not fix.
type FatalError struct{ Msg string }

func (e *FatalError) Error() string { return e.Msg }

// Run keeps the tunnel up until ctx is cancelled or the server rejects the client.
func Run(ctx context.Context, opts Options) error {
	backoff := time.Second
	for {
		connectedAt := time.Now()
		url, sess, err := dial(ctx, opts)
		if err == nil {
			// Keep the same address across reconnects.
			if opts.Subdomain == "" {
				opts.Subdomain = subdomainOf(url)
			}
			if opts.OnConnect != nil {
				opts.OnConnect(url)
			}
			err = serve(ctx, sess, opts.Target)
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

func dial(ctx context.Context, opts Options) (string, *yamux.Session, error) {
	wsURL := strings.TrimSuffix(opts.Server, "/") + protocol.ConnectPath
	wsURL = strings.Replace(strings.Replace(wsURL, "https://", "wss://", 1), "http://", "ws://", 1)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+opts.Token)
	hdr.Set(protocol.HeaderTarget, opts.Target)
	hdr.Set(protocol.HeaderVersion, protocol.Version)
	if opts.Subdomain != "" {
		hdr.Set(protocol.HeaderSubdomain, opts.Subdomain)
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

func serve(ctx context.Context, sess *yamux.Session, target string) error {
	go func() {
		select {
		case <-ctx.Done():
			sess.Close()
		case <-sess.CloseChan():
		}
	}()
	for {
		stream, err := sess.Accept()
		if err != nil {
			return err
		}
		go forward(stream, target)
	}
}

func forward(stream net.Conn, target string) {
	defer stream.Close()
	local, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		msg := "tamsyam: cannot reach " + target + ": " + err.Error() + "\n"
		fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	defer local.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(local, stream); closeWrite(local); done <- struct{}{} }()
	go func() { io.Copy(stream, local); done <- struct{}{} }()
	<-done
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
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
