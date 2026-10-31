<img src="internal/server/web/logo.svg" width="56" alt="">

# tamsyam

**English** · [Русский](README.ru.md)

A self-hosted alternative to ngrok on your own domain: `ts http 8080` → `https://xxxx.t.example.com`. Server, admin panel and CLI.

```
browser ──https──> Caddy (*.t.example.com, wildcard TLS)
                     │ http
                     ▼
               tamsyam-server ──WebSocket + yamux──> ts (CLI) ──> localhost:8080
```

## Features

- **HTTP tunnels** on a random or chosen subdomain; a chosen one is reserved for your token. WebSocket, SSE and HMR work. The terminal shows a live request log and a summary on exit.
- **Password-protected tunnels** (`--auth user:pass`) and **static folders** (`ts http ./dist`, with single-page-app fallback).
- **Config file and service:** several tunnels from `ts.yml` with `ts start`, and `ts service install` to keep them up after reboots.
- **TCP tunnels** (`ts tcp 5432`): the server opens a public port from `TAMSYAM_TCP_PORTS` and returns `tcp://t.example.com:20017`. Good for Postgres, Redis, SSH, RDP or any TCP. A port requested with `-p` is reserved for the token. TCP bypasses Caddy, so forward the range on your router.
- **IP allow lists** (`--allow 203.0.113.0/24,198.51.100.7`) for HTTP and TCP.
- **Request inspector:** the last 200 requests per tunnel with headers and bodies (up to 32 KB each, gzip decoded, JSON pretty-printed). Replay a request or copy it as curl.
- **Statistics:** live counters (rpm, p50/p90, 4xx/5xx, connections, traffic) and 30 days of history: status and traffic charts, top paths, clients by IP.
- **Admin panel:** tunnels, tokens, reserved ports; light and dark theme. The UI is in Russian for now.
- **Access control:** client tokens are stored as sha256 hashes only. The admin panel is password-protected and can be opened without a password from your LAN (`TAMSYAM_PRIVATE_NETWORKS`). Ten failed logins within 15 minutes block the IP.

The inspector shows every header, including `Authorization` and cookies, so keep the admin panel to yourself.

## Install the client

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.ps1 | iex
```

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.sh | sh
```

The script downloads the latest release, verifies its SHA-256 against `checksums.txt` and puts `ts` into `%LOCALAPPDATA%\Programs\tamsyam` (Windows) or `~/.local/bin`. Archives are also on the [Releases](https://github.com/Mi113r/tamsyam/releases) page. With Go: `go install github.com/Mi113r/tamsyam/cmd/ts@latest`.

On Linux, `moreutils` ships its own `ts`: put ours earlier in `PATH` or rename it, e.g. to `tsm`.

## Usage

```bash
ts login https://t.example.com tsm_...
ts http 8080              # random subdomain
ts http 3000 -s myapp     # https://myapp.t.example.com
ts http 192.168.1.5:80    # any host:port
ts http 8080 --allow 203.0.113.0/24   # only from these IPs
ts http 8080 --auth me:secret         # ask for a password
ts http ./dist            # serve a folder

ts tcp 5432               # tcp://t.example.com:<random port>
ts tcp 22 -p 20022        # fixed port, reserved for your token
ts tcp 5432 -q            # no connection log
```

A directory instead of a port is served as static files; unknown paths without an extension fall back to `index.html`. With `--auth`, the server asks for the password and strips it before the request reaches your app.

The client reconnects on its own and keeps the same address. A subdomain or port you ask for explicitly (`-s`, `-p`) is reserved for your token: no other token can take it until you free it in the admin panel.

### Several tunnels: ts.yml

```yaml
# server and token default to ts login
tunnels:
  api:
    addr: 8080
    subdomain: myapi
    auth: me:secret
  site:
    addr: ./dist            # relative to this file
  db:
    proto: tcp
    addr: 5432
    port: 20022
    allow: [203.0.113.0/24]
```

```bash
ts start                  # all tunnels from ./ts.yml
ts start api site         # only these
ts start --config ~/tunnels.yml
```

### Run as a service

```bash
sudo ts service install --config /path/to/ts.yml   # systemd / launchd; elevated PowerShell on Windows
ts service install --user                           # Linux/macOS, without root
ts service status | stop | start | restart | uninstall
```

The service runs `ts start` with that config and the saved login, starts at boot and logs to `ts.log` next to the config file. With `--user` on Linux, run `loginctl enable-linger` so it starts without a login.

## Deploy the server

You need a domain on Cloudflare, a host with a public IP (or ports 80/443 forwarded to it) and a Cloudflare API token with `Zone:DNS:Edit`. The wildcard certificate is issued through DNS-01, so port 80 is not required for ACME.

**DNS.** Create `A t` and `A *.t` records pointing at your public IP, **DNS only** (grey cloud). Cloudflare's Universal SSL does not cover second-level subdomains, and its proxy would cut long-lived connections.

### Docker Compose (easiest)

Two images, for amd64 and arm64: `ghcr.io/mi113r/tamsyam-server` and `ghcr.io/mi113r/tamsyam-caddy` (Caddy with the Cloudflare module and the site config built in).

```bash
mkdir tamsyam && cd tamsyam
curl -fsSLO https://raw.githubusercontent.com/Mi113r/tamsyam/main/deploy/docker/compose.yml
curl -fsSL -o .env https://raw.githubusercontent.com/Mi113r/tamsyam/main/deploy/docker/.env.example
# fill in .env: domain, admin password, Cloudflare token
docker compose up -d
```

Open `https://t.example.com`, create a token under «Токены» (Tokens) and connect the client. For TCP tunnels, forward `TAMSYAM_TCP_PORTS` (default `20000-20019`) on your router. To upgrade: `docker compose pull && docker compose up -d`.

### Binary + systemd (if you already run Caddy)

A step-by-step guide with checks and rollback, written for an AI agent (in Russian): [DEPLOY.md](DEPLOY.md). In short:

1. Build Caddy with the `caddy-dns/cloudflare` module and add the block from [deploy/Caddyfile](deploy/Caddyfile).
2. Install the server:
   ```bash
   useradd -r -s /usr/sbin/nologin tamsyam
   mkdir -p /var/lib/tamsyam /etc/tamsyam && chown tamsyam: /var/lib/tamsyam
   cp tamsyam-server /usr/local/bin/
   cp deploy/server.env.example /etc/tamsyam/server.env   # edit it
   cp deploy/tamsyam-server.service /etc/systemd/system/
   systemctl enable --now tamsyam-server
   ```
3. Open `https://t.example.com` (password from `TAMSYAM_ADMIN_PASSWORD`) and create a token.

### Client IPs, LAN and VPN

The server takes the real client IP from `X-Forwarded-For`, but only from a trusted proxy: loopback or `TAMSYAM_TRUSTED_PROXIES` (already set in the Compose stack). The header is read right to left, so values a client adds itself are ignored. If Caddy runs on another host, list its IP in `TAMSYAM_TRUSTED_PROXIES`; otherwise every request looks like it came from Caddy.

`TAMSYAM_PRIVATE_NETWORKS` (comma-separated CIDRs/IPs) opens the admin panel without a password from those networks. Routers often masquerade VPN clients behind their own LAN address, so measure which addresses the server actually sees first. [AGENTS.md](AGENTS.md) walks through it with a verification checklist.

Operating a running server (upgrades, passwords, tokens, networks) is covered in [AGENTS.md](AGENTS.md) (in Russian). Every setting is listed and explained in [deploy/server.env.example](deploy/server.env.example).

## Development

```bash
go build -o bin/ ./cmd/...
go test ./...
TAMSYAM_DOMAIN=localhost TAMSYAM_ADMIN_PASSWORD=dev TAMSYAM_PUBLIC_SCHEME=http TAMSYAM_PUBLIC_PORT=8090 bin/tamsyam-server
# admin: http://localhost:8090, tunnels: http://<sub>.localhost:8090
ts http 8080 --server http://localhost:8090 --token tsm_...
```

Pushing a `vX.Y.Z` tag releases the client and server through GoReleaser and publishes the Docker images to GHCR.

## Roadmap

- Inspector: edit before replay, live updates, filters.
- Webhook capture: keep requests while the client is offline and replay them later.
- Tunnels without a client (`ssh -R`), SSH access to a machine without sshd.
- Custom domains per tunnel, traffic rules (headers, CORS, rate limits), OAuth in front of a tunnel.
- Per-token limits, client geolocation via GeoLite2.
- Users and OAuth in the admin panel instead of a single password, Prometheus metrics.

## License

[MIT](LICENSE)
