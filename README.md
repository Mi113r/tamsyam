# tamsyam

Свой аналог tuna/ngrok: `tamsyam http 8080` → `https://xxxx.t.example.com`.

```
браузер ──https──> Caddy (*.t.example.com, wildcard TLS)
                     │ http
                     ▼
               tamsyam-server ──WebSocket + yamux──> tamsyam (CLI) ──> localhost:8080
```

- **tamsyam-server** живёт за Caddy. Запросы к `t.example.com` уходят в админку и на endpoint клиентов, запросы к `<sub>.t.example.com` уходят в туннель.
- **tamsyam** — CLI. Он подключается по WebSocket (проходит через любой прокси и 443), поднимает поверх yamux и проксирует каждый TCP-поток на локальный порт. WebSocket, SSE и HMR работают.
- Токены хранятся в SQLite (только sha256-хэш). Админка показывает активные туннели, умеет создавать и отзывать токены и отключать туннели.
- **Инспектор запросов:** для каждого туннеля хранятся последние 200 запросов в памяти сервера: заголовки, тела запроса и ответа (до 32 КБ каждое, gzip распаковывается, JSON форматируется). Запрос можно повторить одной кнопкой или скопировать как curl. В инспекторе видны все заголовки, включая `Authorization` и cookies, поэтому админка должна быть доступна только вам.

## Сборка

```bash
go build -o bin/ ./cmd/...
```

Релиз: `git tag v0.1.0 && git push origin v0.1.0`. GitHub Actions соберёт через GoReleaser клиент (Linux, macOS, Windows) и сервер (Linux) и выложит архивы в Releases.

Кросс-компиляция сервера под Linux вручную:

```bash
GOOS=linux GOARCH=amd64 go build -o bin/tamsyam-server ./cmd/tamsyam-server
```

## Развёртывание (Proxmox + Caddy + Cloudflare)

Подробная пошаговая инструкция с проверками и откатом, рассчитанная на агента: [DEPLOY.md](DEPLOY.md). Ниже краткая версия.

1. **DNS в Cloudflare:** записи `A t` и `A *.t` на белый IP, режим **DNS only** (серое облако). Universal SSL Cloudflare не покрывает поддомены второго уровня, и прокси Cloudflare обрывал бы долгие соединения.
2. **Caddy:** соберите с модулем `caddy-dns/cloudflare` и возьмите [deploy/Caddyfile](deploy/Caddyfile). Нужен API-токен с правом `Zone:DNS:Edit`.
3. **Сервер** (LXC или VM):
   ```bash
   useradd -r -s /usr/sbin/nologin tamsyam
   mkdir -p /var/lib/tamsyam /etc/tamsyam && chown tamsyam: /var/lib/tamsyam
   cp tamsyam-server /usr/local/bin/
   cp deploy/server.env.example /etc/tamsyam/server.env   # отредактировать
   cp deploy/tamsyam-server.service /etc/systemd/system/
   systemctl enable --now tamsyam-server
   ```
4. Откройте `https://t.example.com` (пароль из `TAMSYAM_ADMIN_PASSWORD`) и создайте токен.

## Использование

```bash
tamsyam login https://t.example.com tsm_...
tamsyam http 8080              # случайный поддомен
tamsyam http 3000 -s myapp     # https://myapp.t.example.com
tamsyam http 192.168.1.5:80    # любой host:port
```

При обрыве клиент переподключается и сохраняет тот же адрес.

## Локальный запуск для разработки

```bash
TAMSYAM_DOMAIN=localhost TAMSYAM_ADMIN_PASSWORD=dev TAMSYAM_PUBLIC_SCHEME=http TAMSYAM_PUBLIC_PORT=8090 bin/tamsyam-server
# админка: http://localhost:8090, туннели: http://<sub>.localhost:8090
tamsyam http 8080 --server http://localhost:8090 --token tsm_...
```

## Идеи для развития

- Инспектор: редактирование запроса перед повтором, live-обновление через SSE, фильтры.
- Закрепление поддоменов за токеном, лимиты на токен.
- Basic-auth или IP-allowlist на туннель (`tamsyam http 8080 --auth user:pass`).
- TCP-туннели (`tamsyam tcp 5432`) на выделенных портах.
- Пользователи и OAuth в админке вместо одного пароля, метрики Prometheus.
