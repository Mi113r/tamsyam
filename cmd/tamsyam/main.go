// Command tamsyam exposes a local HTTP service through a tamsyam server.
//
//	tamsyam login https://t.example.com tsm_xxx
//	tamsyam http 8080
//	tamsyam http 3000 -s myapp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mi113r/tamsyam/internal/client"
	"github.com/Mi113r/tamsyam/internal/protocol"
)

type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

const usage = `tamsyam — expose a local service to the internet

Usage:
  tamsyam login <server-url> <token>   save server and token
  tamsyam http <port|host:port> [-s subdomain] [--server url] [--token t]
  tamsyam version
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
	case "http":
		err = cmdHTTP(os.Args[2:])
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
		return errors.New("usage: tamsyam login <server-url> <token>")
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

func cmdHTTP(args []string) error {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	sub := fs.String("s", "", "requested subdomain")
	server := fs.String("server", "", "server URL (overrides saved config)")
	token := fs.String("token", "", "access token (overrides saved config)")

	// Allow flags both before and after the positional target.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: tamsyam http <port|host:port> [-s subdomain]")
	}
	target := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	if !strings.Contains(target, ":") {
		target = "localhost:" + target
	}

	cfg := loadConfig()
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if cfg.Server == "" || cfg.Token == "" {
		return errors.New("not logged in: run `tamsyam login <server-url> <token>` first")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("tamsyam %s · connecting to %s\n", protocol.Version, cfg.Server)
	return client.Run(ctx, client.Options{
		Server:    cfg.Server,
		Token:     cfg.Token,
		Subdomain: *sub,
		Target:    target,
		OnConnect: func(url string) {
			fmt.Printf("\n  Forwarding  %s  ->  http://%s\n\n  Ctrl+C to stop\n\n", url, target)
		},
		OnDisconnect: func(err error, retry time.Duration) {
			fmt.Printf("connection lost (%v), reconnecting in %s...\n", err, retry)
		},
	})
}

func configPath() (string, error) {
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
		json.Unmarshal(data, &cfg)
	}
	return cfg
}
