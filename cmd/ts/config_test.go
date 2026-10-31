package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ts.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFileConfig(t *testing.T) {
	path := writeConfig(t, `
server: https://t.example.com
tunnels:
  api:
    addr: 8080
    subdomain: myapi
    auth: me:secret
    allow: 10.0.0.0/8, 127.0.0.1
  site:
    addr: ./dist
  db:
    proto: tcp
    addr: 5432
    port: 20022
    allow: [203.0.113.0/24, 198.51.100.7]
`)
	fc, err := loadFileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := selectTunnels(fc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 || specs[0].Name != "api" || specs[1].Name != "db" || specs[2].Name != "site" {
		t.Fatalf("specs: %+v", specs)
	}
	api, db, site := specs[0], specs[1], specs[2]
	if api.Proto != "http" || api.Addr != "8080" || api.Auth != "me:secret" || api.Allow.String() != "10.0.0.0/8, 127.0.0.1" {
		t.Fatalf("api: %+v", api)
	}
	if db.Proto != "tcp" || db.Port != "20022" || db.Allow.String() != "203.0.113.0/24,198.51.100.7" {
		t.Fatalf("db: %+v", db)
	}
	// Directories resolve against the config file, wherever ts runs from.
	if site.Addr != filepath.Join(filepath.Dir(path), "dist") {
		t.Fatalf("site addr: %q", site.Addr)
	}

	if _, err := selectTunnels(fc, []string{"nope"}); err == nil {
		t.Fatal("unknown tunnel name accepted")
	}
	only, _ := selectTunnels(fc, []string{"site", "api"})
	if len(only) != 2 || only[0].Name != "api" {
		t.Fatalf("selected: %+v", only)
	}
}

func TestLoadFileConfigErrors(t *testing.T) {
	for body, want := range map[string]string{
		"tunnels: {}\n": "no tunnels",
		"tunnels:\n  a: {addr: 1, protoo: tcp}\n": "protoo",
		"tunnels:\n  a: {addr: 1, proto: udp}\n":  "proto must be",
	} {
		_, err := loadFileConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	for addr, want := range map[string]string{"8080": "localhost:8080", "10.0.0.5:80": "10.0.0.5:80"} {
		got, _, _, err := resolveTarget(tunnelSpec{Proto: "http", Addr: addr})
		if err != nil || got != want {
			t.Errorf("%s: %q, %v", addr, got, err)
		}
	}
	dir := t.TempDir()
	got, local, stop, err := resolveTarget(tunnelSpec{Proto: "http", Addr: dir})
	if err != nil || !strings.HasPrefix(got, "127.0.0.1:") || !strings.Contains(local, dir) {
		t.Fatalf("dir: %q %q %v", got, local, err)
	}
	stop()
}
