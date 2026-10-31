package server

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Mi113r/tamsyam/internal/protocol"
)

var (
	errTCPDisabled  = errors.New("TCP tunnels are disabled on this server")
	errPortRange    = errors.New("port is outside the server's TCP port range")
	errPortReserved = errors.New("port is reserved by another token")
	errNoFreePorts  = errors.New("no free TCP ports left")
)

// ConnEntry is one finished TCP connection in the tunnel's connection log.
type ConnEntry struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	RemoteIP   string    `json:"remote_ip"`
	DurationMs int64     `json:"duration_ms"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`
	Denied     bool      `json:"denied,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// listenTCP opens the public listener for a TCP tunnel. requested is the port
// the client asked for, or empty for a random free one. An explicitly
// requested port is reserved for the token (unless reserve is false, as on a
// client reconnect), so the address survives restarts.
func (s *Server) listenTCP(tokenID int64, requested string, reserve bool) (net.Listener, int, error) {
	lo, hi := s.cfg.TCPPortMin, s.cfg.TCPPortMax
	if lo == 0 {
		return nil, 0, errTCPDisabled
	}

	if requested != "" {
		port, err := strconv.Atoi(requested)
		if err != nil || port < lo || port > hi {
			return nil, 0, fmt.Errorf("%w (%d-%d)", errPortRange, lo, hi)
		}
		if owner, ok, err := s.store.PortOwner(port); err != nil {
			return nil, 0, err
		} else if ok && owner != tokenID {
			return nil, 0, errPortReserved
		}
		if s.registry.Taken(tcpKey(port), tokenID) {
			return nil, 0, errPortReserved
		}
		// Same token reconnecting: free the port held by its previous session.
		if old := s.registry.Get(tcpKey(port)); old != nil {
			old.Close()
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.TCPBind, strconv.Itoa(port)))
		if err != nil {
			return nil, 0, fmt.Errorf("port %d is unavailable: %w", port, err)
		}
		if !reserve {
			return ln, port, nil
		}
		if owner, err := s.store.ReservePort(port, tokenID); err != nil || owner != tokenID {
			ln.Close()
			if err == nil {
				err = errPortReserved
			}
			return nil, 0, err
		}
		return ln, port, nil
	}

	for range 64 {
		port := lo + rand.IntN(hi-lo+1)
		if s.registry.Get(tcpKey(port)) != nil {
			continue
		}
		if owner, ok, err := s.store.PortOwner(port); err != nil || (ok && owner != tokenID) {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.TCPBind, strconv.Itoa(port)))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, errNoFreePorts
}

func (t *Tunnel) serveTCP() {
	for {
		c, err := t.listener.Accept()
		if err != nil {
			return
		}
		go t.handleConn(c)
	}
}

func (t *Tunnel) handleConn(c net.Conn) {
	defer c.Close()
	start := time.Now()
	ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	entry := ConnEntry{ID: t.seq.Add(1), Time: start.UTC(), RemoteIP: ip}

	defer func() {
		entry.DurationMs = time.Since(start).Milliseconds()
		t.recordConn(entry)
		if entry.Denied {
			return
		}
		t.stats.record(sample{tunnel: t.Key, at: start, bytesIn: entry.BytesIn, bytesOut: entry.BytesOut, remoteIP: ip})
		t.emit(protocol.Event{
			Type: "conn", Time: entry.Time, RemoteIP: ip, DurationMs: entry.DurationMs,
			BytesIn: entry.BytesIn, BytesOut: entry.BytesOut, Error: entry.Error,
		})
	}()

	if !t.allowed(ip) {
		entry.Denied = true
		return
	}
	t.live.totalConns.Add(1)
	t.live.activeConns.Add(1)
	defer t.live.activeConns.Add(-1)

	stream, err := t.session.Open()
	if err != nil {
		entry.Error = "tunnel unavailable"
		return
	}
	defer stream.Close()

	in := &countingWriter{w: stream, live: &t.live.bytesIn}
	out := &countingWriter{w: c, live: &t.live.bytesOut}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(in, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(out, stream); done <- struct{}{} }()
	<-done
	c.Close()
	stream.Close()
	<-done
	entry.BytesIn, entry.BytesOut = in.n, out.n
}

func (t *Tunnel) recordConn(e ConnEntry) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	if len(t.conns) >= requestLogSize {
		t.conns = t.conns[1:]
	}
	t.conns = append(t.conns, e)
}

// ConnLog returns finished TCP connections, newest first.
func (t *Tunnel) ConnLog() []ConnEntry {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	out := make([]ConnEntry, len(t.conns))
	for i, e := range t.conns {
		out[len(t.conns)-1-i] = e
	}
	return out
}

// countingWriter counts bytes per connection and in the tunnel's live totals.
type countingWriter struct {
	w    io.Writer
	n    int64
	live *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.live.Add(int64(n))
	return n, err
}
