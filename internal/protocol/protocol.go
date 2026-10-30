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

const (
	Version     = "0.1.0"
	ConnectPath = "/_tamsyam/connect"

	// Request headers (client -> server).
	HeaderSubdomain = "X-Tamsyam-Subdomain"
	HeaderTarget    = "X-Tamsyam-Target"
	HeaderVersion   = "X-Tamsyam-Version"

	// Response headers (server -> client) on a successful upgrade.
	HeaderURL = "X-Tamsyam-URL"
)

// YamuxConfig returns the multiplexer settings both ends must agree on.
func YamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.MaxStreamWindowSize = 1 << 20
	cfg.LogOutput = io.Discard
	return cfg
}
