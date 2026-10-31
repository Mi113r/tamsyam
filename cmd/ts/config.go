package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

// fileConfig is ts.yml: several tunnels started together by `ts start`.
//
//	server: https://t.example.com   # optional, defaults to ts login
//	token: tsm_...                  # optional, defaults to ts login
//	tunnels:
//	  api:  {proto: http, addr: 8080, subdomain: myapi, auth: "me:secret"}
//	  site: {proto: http, addr: ./dist}
//	  db:   {proto: tcp, addr: 5432, port: 20022, allow: [203.0.113.0/24]}
type fileConfig struct {
	Server  string                `yaml:"server"`
	Token   string                `yaml:"token"`
	Tunnels map[string]tunnelSpec `yaml:"tunnels"`
}

type tunnelSpec struct {
	Name      string     `yaml:"-"`
	Proto     string     `yaml:"proto"` // http (default) or tcp
	Addr      string     `yaml:"addr"`  // port, host:port or (http) a directory
	Subdomain string     `yaml:"subdomain"`
	Port      string     `yaml:"port"`
	Allow     stringList `yaml:"allow"`
	Auth      string     `yaml:"auth"`
}

// stringList accepts both "a,b" and [a, b] in YAML.
type stringList []string

func (l *stringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = stringList{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

func (l stringList) String() string {
	var parts []string
	for _, v := range l {
		if v = strings.TrimSpace(v); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ",")
}

// findConfig returns the explicit path, else ./ts.yml, else ts.yml next to the saved login.
func findConfig(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	candidates := []string{"ts.yml", "ts.yaml"}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "tamsyam", "ts.yml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}
	return "", fmt.Errorf("no config file: create ts.yml (see `ts start --help`) or pass --config")
}

func loadFileConfig(path string) (fileConfig, error) {
	var fc fileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return fc, err
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		return fc, fmt.Errorf("%s: %w", path, err)
	}
	if len(fc.Tunnels) == 0 {
		return fc, fmt.Errorf("%s: no tunnels defined", path)
	}
	for name, sp := range fc.Tunnels {
		sp.Name = name
		if sp.Proto == "" {
			sp.Proto = "http"
		}
		if sp.Proto != "http" && sp.Proto != "tcp" {
			return fc, fmt.Errorf("%s: tunnel %q: proto must be http or tcp", path, name)
		}
		// Relative directories are relative to the config file, not the cwd.
		if sp.Proto == "http" && !filepath.IsAbs(sp.Addr) && !strings.Contains(sp.Addr, ":") {
			dir := filepath.Join(filepath.Dir(path), sp.Addr)
			if strings.Trim(sp.Addr, "0123456789") != "" && isDirArg(dir) {
				sp.Addr = dir
			}
		}
		fc.Tunnels[name] = sp
	}
	return fc, nil
}

// selectTunnels returns the named tunnels (all when names is empty), sorted by name.
func selectTunnels(fc fileConfig, names []string) ([]tunnelSpec, error) {
	if len(names) == 0 {
		for name := range fc.Tunnels {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	specs := make([]tunnelSpec, 0, len(names))
	for _, name := range names {
		sp, ok := fc.Tunnels[name]
		if !ok {
			return nil, fmt.Errorf("no tunnel %q in the config", name)
		}
		specs = append(specs, sp)
	}
	return specs, nil
}

const startUsage = `Usage: ts start [name ...] [--config ts.yml] [-q]

Runs the tunnels from a config file (all of them, or only the named ones).
Without --config, ts looks for ./ts.yml, then ts.yml next to the saved login.

Example ts.yml:

  # server and token default to ts login
  tunnels:
    api:
      addr: 8080
      subdomain: myapi
      auth: me:secret
    site:
      addr: ./dist            # a folder is served as static files
    db:
      proto: tcp
      addr: 5432
      port: 20022
      allow: [203.0.113.0/24]
`

func cmdStart(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, startUsage) }
	cfgPath := fs.String("config", "", "config file")
	quiet := fs.Bool("q", false, "do not print requests and connections")
	asService := fs.Bool("service", false, "run under the OS service manager (set by ts service install)")
	fs.StringVar(&loginFile, "login-file", "", "saved login to use")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := findConfig(*cfgPath)
	if err != nil {
		return err
	}
	run := func(ctx context.Context) error {
		fc, err := loadFileConfig(path)
		if err != nil {
			return err
		}
		specs, err := selectTunnels(fc, fs.Args())
		if err != nil {
			return err
		}
		login, err := resolveLogin(config{Server: fc.Server, Token: fc.Token})
		if err != nil {
			return err
		}
		return runTunnels(ctx, login, specs, *quiet)
	}
	if *asService {
		return runService(path, run)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
