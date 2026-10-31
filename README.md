<img src="internal/server/web/logo.svg" width="56" alt="">

# tamsyam

Свой аналог tuna/ngrok: `ts http 8080` → `https://xxxx.t.example.com`.

Клиентская команда называется `ts`. На Linux с пакетом `moreutils` уже есть своя утилита `ts`: тогда положите наш бинарник раньше в `PATH` или переименуйте его, например в `tsm`.

```
браузер ──https──> Caddy (*.t.example.com, wildcard TLS)
                     │ http
                     ▼
               tamsyam-server ──WebSocket + yamux──> ts (CLI) ──> localhost:8080
```

## Установка клиента

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.ps1 | iex
```

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.sh | sh
```

Скрипт скачивает последний релиз, сверяет SHA-256 с `checksums.txt` и кладёт `ts` в `%LOCALAPPDATA%\Programs\tamsyam` (Windows) или `~/.local/bin`. Архивы можно скачать и вручную со страницы [Releases](https://github.com/Mi113r/tamsyam/releases). Если установлен Go: `go install github.com/Mi113r/tamsyam/cmd/ts@latest`.

Сервер нужен свой: см. раздел «Развёртывание» ниже.

- **tamsyam-server** живёт за Caddy. Запросы к `t.example.com` уходят в админку и на endpoint клиентов, запросы к `<sub>.t.example.com` уходят в туннель.
- **ts** — CLI. Он подключается по WebSocket (проходит через любой прокси и 443), поднимает поверх yamux и проксирует каждый TCP-поток на локальный порт. WebSocket, SSE и HMR работают. В терминале печатается живой лог запросов и соединений, при выходе — сводка.
- **TCP-туннели** (`ts tcp 5432`): сервер открывает публичный порт из диапазона `TAMSYAM_TCP_PORTS` и отдаёт адрес `tcp://t.example.com:20017`. Подходит для Postgres, Redis, SSH, RDP и любого TCP. Порт, заданный через `-p`, закрепляется за токеном и не меняется после перезапуска. TCP идёт мимо Caddy, поэтому диапазон нужно пробросить на роутере.
- **IP-фильтр** (`--allow 203.0.113.0/24,198.51.100.7`): туннель доступен только с этих адресов, для HTTP и TCP.
- **Статистика:** живые счётчики туннеля (rpm, p50/p90, 4xx/5xx, активные соединения, трафик) и история за 30 дней в SQLite: графики запросов по статусам и трафика за 1 ч / 24 ч / 7 / 30 дней, популярные пути, клиенты по IP, уникальные IP.
- Тема админки: системная, светлая или тёмная (переключатель рядом с логотипом, выбор запоминается в браузере).
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

Админке можно доверить домашнюю сеть: задайте `TAMSYAM_PRIVATE_NETWORKS` (список CIDR/IP через запятую) — оттуда панель открывается без пароля. Все остальные адреса, включая VPN-клиентов, вводят пароль как обычно.

Реальный IP клиента сервер берёт из `X-Forwarded-For`, но только если запрос пришёл от доверенного прокси: loopback или адреса из `TAMSYAM_TRUSTED_PROXIES`. Заголовок читается справа налево, поэтому подставленные клиентом значения не учитываются. Если Caddy работает на другом хосте, укажите его IP в `TAMSYAM_TRUSTED_PROXIES`. Без этого все запросы выглядят как пришедшие от Caddy: беспарольный вход из LAN не сработает, а `--allow` и статистика будут видеть адрес Caddy.

## Использование

```bash
ts login https://t.example.com tsm_...
ts http 8080              # случайный поддомен
ts http 3000 -s myapp     # https://myapp.t.example.com
ts http 192.168.1.5:80    # любой host:port
ts http 8080 --allow 203.0.113.0/24   # только с этих IP

ts tcp 5432               # tcp://t.example.com:<случайный порт>
ts tcp 22 -p 20022        # фиксированный порт, закрепляется за токеном
ts tcp 5432 -q            # без лога соединений
```

При обрыве клиент переподключается и сохраняет тот же адрес. Закреплённые порты видны в админке, там же их можно освободить.

## Локальный запуск для разработки

```bash
TAMSYAM_DOMAIN=localhost TAMSYAM_ADMIN_PASSWORD=dev TAMSYAM_PUBLIC_SCHEME=http TAMSYAM_PUBLIC_PORT=8090 bin/tamsyam-server
# админка: http://localhost:8090, туннели: http://<sub>.localhost:8090
ts http 8080 --server http://localhost:8090 --token tsm_...
```

## Идеи для развития

- Инспектор: редактирование запроса перед повтором, live-обновление через SSE, фильтры.
- Закрепление поддоменов за токеном, лимиты на токен.
- Basic-auth и rate limit на туннель (`ts http 8080 --auth user:pass`).
- Инспектор для Postgres/Redis поверх TCP-туннеля, SSH-туннель без SSH-сервера.
- `ts http --file-server ./dist`, география клиентов по GeoLite2.
- Пользователи и OAuth в админке вместо одного пароля, метрики Prometheus.
