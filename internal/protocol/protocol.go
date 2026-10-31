// Package protocol holds the wire contract shared by the server and the CLI client.
//
// A client opens a WebSocket to ConnectPath on the server's base domain,
// authenticating with a bearer token. The server answers the upgrade with the
// assigned public URL in response headers, then runs a yamux session over the
// WebSocket: the server opens one stream per incoming TCP connection and the
// client pipes each stream to the local target.
package protocol

import (
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// Version is set at release time via -ldflags "-X .../protocol.Version=...".
var Version = "dev"

const (
	ConnectPath = "/_tamsyam/connect"

	// Request headers (client -> server).
	HeaderProto = "X-Tamsyam-Proto" // "http" (default) or "tcp"
	HeaderPort  = "X-Tamsyam-Port"  // requested public TCP port
	HeaderAllow = "X-Tamsyam-Allow" // comma-separated IPs/CIDRs allowed to connect
	// HeaderReconnect marks a reconnect to the previously assigned port or subdomain, which
	// must not create a permanent port reservation.
	HeaderReconnect = "X-Tamsyam-Reconnect"
	HeaderSubdomain = "X-Tamsyam-Subdomain"
	// HeaderAuth is "user:password" for HTTP basic auth in front of the tunnel.
	HeaderAuth    = "X-Tamsyam-Auth"
	HeaderTarget  = "X-Tamsyam-Target"
	HeaderVersion = "X-Tamsyam-Version"

	// Response headers (server -> client) on a successful upgrade.
	HeaderURL = "X-Tamsyam-URL"
)

// Event is streamed to the client over the control stream, one JSON object per
// line. The client opens the control stream right after the session starts;
// all other streams are opened by the server and carry tunnel traffic.
type Event struct {
	Type       string    `json:"type"` // "request" or "conn"
	Time       time.Time `json:"time"`
	RemoteIP   string    `json:"remote_ip"`
	DurationMs int64     `json:"duration_ms"`
	BytesIn    int64     `json:"bytes_in"`
	BytesOut   int64     `json:"bytes_out"`

	// HTTP only.
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Status int    `json:"status,omitempty"`

	// TCP only: set when the connection could not be served.
	Error string `json:"error,omitempty"`
}

// YamuxConfig returns the multiplexer settings both ends must agree on.
func YamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.MaxStreamWindowSize = 1 << 20
	cfg.LogOutput = io.Discard
	return cfg
}
