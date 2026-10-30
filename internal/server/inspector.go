package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
	"unicode/utf8"
)

// maxCapturedBody bounds memory: requestLogSize * 2 * maxCapturedBody per tunnel at worst.
const maxCapturedBody = 32 << 10

var (
	errRequestNotFound = errors.New("request not found")
	errNotReplayable   = errors.New("request cannot be replayed")
)

// RequestEntry is the summary shown in the request list.
type RequestEntry struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"duration_ms"`
	RemoteIP   string    `json:"remote_ip"`
	Replay     bool      `json:"replay"`
	Upgrade    bool      `json:"upgrade"`
}

// RequestDetail is the full captured exchange.
type RequestDetail struct {
	RequestEntry
	RequestHeaders  http.Header `json:"request_headers"`
	RequestBody     Body        `json:"request_body"`
	ResponseHeaders http.Header `json:"response_headers"`
	ResponseBody    Body        `json:"response_body"`
}

type Body struct {
	Size      int64  `json:"size"`
	Data      string `json:"data"`
	Encoding  string `json:"encoding"` // "text" or "base64"
	Truncated bool   `json:"truncated"`
	Decoded   string `json:"decoded,omitempty"` // content-encoding removed for display, e.g. "gzip"
}

// exchange is one recorded request/response pair.
type exchange struct {
	RequestEntry
	reqHeaders  http.Header
	respHeaders http.Header
	reqBody     *capBuffer
	respBody    *capBuffer
}

func (e *exchange) detail() RequestDetail {
	d := RequestDetail{
		RequestEntry:    e.RequestEntry,
		RequestHeaders:  e.reqHeaders,
		ResponseHeaders: e.respHeaders,
		RequestBody:     e.reqBody.body(""),
	}
	d.ResponseBody = e.respBody.body(e.respHeaders.Get("Content-Encoding"))
	return d
}

// capBuffer keeps the first maxCapturedBody bytes written to it. It is locked
// because the transport may still be reading a request body after the
// response has been delivered.
type capBuffer struct {
	mu        sync.Mutex
	buf       []byte
	size      int64
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	n := len(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.size += int64(n)
	room := maxCapturedBody - len(c.buf)
	if n > room {
		c.truncated = true
		p = p[:max(room, 0)]
	}
	c.buf = append(c.buf, p...)
	return n, nil
}

func (c *capBuffer) bytes() ([]byte, bool) {
	data, _, truncated := c.snapshot()
	return data, truncated
}

func (c *capBuffer) snapshot() ([]byte, int64, bool) {
	if c == nil {
		return nil, 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf), c.size, c.truncated
}

func (c *capBuffer) body(contentEncoding string) Body {
	data, size, truncated := c.snapshot()
	b := Body{Size: size, Truncated: truncated, Encoding: "text"}

	if contentEncoding == "gzip" && !truncated && len(data) > 0 {
		if zr, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
			if plain, err := io.ReadAll(io.LimitReader(zr, 4*maxCapturedBody)); err == nil {
				data, b.Decoded = plain, "gzip"
			}
		}
	}
	if utf8.Valid(data) {
		b.Data, b.Encoding = string(data), "text"
	} else {
		b.Data, b.Encoding = base64.StdEncoding.EncodeToString(data), "base64"
	}
	return b
}

// teeBody copies a request body into a capBuffer as the proxy reads it.
type teeBody struct {
	io.ReadCloser
	buf *capBuffer
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	_, _ = t.buf.Write(p[:n])
	return n, err
}

// statusRecorder captures status, headers and the start of the body on the way to the client.
type statusRecorder struct {
	http.ResponseWriter
	status int
	header http.Header
	body   *capBuffer
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
		s.header = s.ResponseWriter.Header().Clone()
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.WriteHeader(http.StatusOK)
	}
	_, _ = s.body.Write(p)
	return s.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach Flush/Hijack on the real writer,
// which ReverseProxy needs for streaming and WebSocket upgrades.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Replay re-sends a recorded request through the tunnel and returns the new entry's ID.
func (t *Tunnel) Replay(ctx context.Context, id uint64) (uint64, error) {
	ex := t.find(id)
	if ex == nil {
		return 0, errRequestNotFound
	}
	body, truncated := ex.reqBody.bytes()
	if ex.Upgrade || truncated {
		return 0, errNotReplayable
	}
	req, err := http.NewRequestWithContext(ctx, ex.Method, "http://"+ex.Host+ex.Path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header = ex.reqHeaders.Clone()
	req.Host = ex.Host
	req.RemoteAddr = "replay:0"
	return t.serve(httptest.NewRecorder(), req, true), nil
}
