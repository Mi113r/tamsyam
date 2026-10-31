// Command ts (tamsyam client) exposes local services through a tamsyam server.
//
//	ts login https://t.example.com tsm_xxx
//	ts http 8080
//	ts http 3000 -s myapp
//	ts tcp 5432 -p 20017
//	ts start            # tunnels from ts.yml
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Mi113r/tamsyam/internal/protocol"
)

// config is the saved login (ts login).
type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// out receives everything ts prints; the service mode also copies it to a log file.
var out io.Writer = os.Stdout

const usage = `ts — expose a local service to the internet (tamsyam client)

Usage:
  ts login <server-url> <token>   save server and token
  ts http <port|host:port|dir> [-s subdomain] [--auth user:pass] [--allow cidr,...] [-q]
  ts tcp  <port|host:port> [-p public-port] [--allow cidr,...] [-q]
  ts start [name ...] [--config ts.yml] [-q]   run tunnels from a config file
  ts service install|uninstall|start|stop|restart|status [--config ts.yml] [--user]
  ts version

Flags for http/tcp:
  -s, -p      fixed subdomain / public TCP port, reserved for your token
  --auth      require HTTP basic auth (user:password) in front of the tunnel
  --allow     only these IPs/CIDRs may use the tunnel, e.g. 203.0.113.0/24,198.51.100.7
  -q          do not print requests and connections
  --server, --token   override the saved login

A directory instead of a port (ts http ./dist) is served as static files.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(os.Args[2:])
	case "http", "tcp":
		err = cmdTunnel(os.Args[1], os.Args[2:])
	case "start":
		err = cmdStart(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("tamsyam", protocol.Version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdLogin(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: ts login <server-url> <token>")
	}
	cfg := config{Server: strings.TrimSuffix(args[0], "/"), Token: args[1]}
	if !strings.HasPrefix(cfg.Server, "http://") && !strings.HasPrefix(cfg.Server, "https://") {
		cfg.Server = "https://" + cfg.Server
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Println("Saved to", path)
	return nil
}

func cmdTunnel(proto string, args []string) error {
	fs := flag.NewFlagSet(proto, flag.ContinueOnError)
	spec := tunnelSpec{Proto: proto}
	if proto == "http" {
		fs.StringVar(&spec.Subdomain, "s", "", "requested subdomain (reserved for your token)")
		fs.StringVar(&spec.Auth, "auth", "", "require basic auth, user:password")
	} else {
		fs.StringVar(&spec.Port, "p", "", "public TCP port (reserved for your token)")
	}
	allow := fs.String("allow", "", "comma-separated IPs/CIDRs allowed to connect")
	quiet := fs.Bool("q", false, "do not print requests and connections")
	server := fs.String("server", "", "server URL (overrides saved config)")
	token := fs.String("token", "", "access token (overrides saved config)")

	// Allow flags both before and after the positional target.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: ts %s <port|host:port>", proto)
	}
	spec.Addr = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	spec.Allow = stringList{*allow}

	login, err := resolveLogin(config{Server: *server, Token: *token})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runTunnels(ctx, login, []tunnelSpec{spec}, *quiet)
}

// resolveLogin fills what override leaves empty from the saved login.
func resolveLogin(override config) (config, error) {
	cfg := loadConfig()
	if override.Server != "" {
		cfg.Server = override.Server
	}
	if override.Token != "" {
		cfg.Token = override.Token
	}
	if cfg.Server == "" || cfg.Token == "" {
		return cfg, errors.New("not logged in: run `ts login <server-url> <token>` first")
	}
	return cfg, nil
}

func formatEvent(ev protocol.Event) string {
	at := ev.Time.Local().Format("15:04:05")
	if ev.Type == "conn" {
		line := fmt.Sprintf("%s  %-15s  %8s  in %-9s out %s", at, ev.RemoteIP,
			formatDuration(ev.DurationMs), formatBytes(ev.BytesIn), formatBytes(ev.BytesOut))
		if ev.Error != "" {
			line += "  (" + ev.Error + ")"
		}
		return line
	}
	return fmt.Sprintf("%s  %-6s %3d  %8s  %s", at, ev.Method, ev.Status, formatDuration(ev.DurationMs), ev.Path)
}

// summary accumulates what the tunnel served, printed when the client exits.
type summary struct {
	mu                sync.Mutex
	requests, s4, s5  int
	conns             int
	bytesIn, bytesOut int64
	durs              []int64
}

func (s *summary) add(ev protocol.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytesIn += ev.BytesIn
	s.bytesOut += ev.BytesOut
	if ev.Type == "conn" {
		s.conns++
		return
	}
	s.requests++
	switch ev.Status / 100 {
	case 4:
		s.s4++
	case 5:
		s.s5++
	}
	if len(s.durs) < 100_000 {
		s.durs = append(s.durs, ev.DurationMs)
	}
}

func (s *summary) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	if s.requests > 0 {
		d := slices.Clone(s.durs)
		slices.Sort(d)
		parts = append(parts, fmt.Sprintf("Requests: %d · 4xx: %d · 5xx: %d · p50 %s · p90 %s",
			s.requests, s.s4, s.s5, formatDuration(d[len(d)*50/100]), formatDuration(d[min(len(d)*90/100, len(d)-1)])))
	}
	if s.conns > 0 {
		parts = append(parts, fmt.Sprintf("Connections: %d", s.conns))
	}
	if s.requests > 0 || s.conns > 0 {
		parts = append(parts, fmt.Sprintf("Traffic: in %s · out %s", formatBytes(s.bytesIn), formatBytes(s.bytesOut)))
	}
	return strings.Join(parts, "\n")
}

func formatDuration(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d ms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", ms/60_000, ms/1000%60)
	}
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// loginFile overrides the saved login location (the service passes the
// installing user's file, since it runs as another account).
var loginFile string

func configPath() (string, error) {
	if loginFile != "" {
		return loginFile, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tamsyam", "config.json"), nil
}

func loadConfig() config {
	var cfg config
	path, err := configPath()
	if err != nil {
		return cfg
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &cfg)
	}
	return cfg
}
