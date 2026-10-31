<img src="internal/server/web/logo.svg" width="56" alt="">

# tamsyam

[English](README.md) · **Русский**

Свой аналог tuna/ngrok на своём домене: `ts http 8080` → `https://xxxx.t.example.com`. Сервер, админка и CLI.

```
браузер ──https──> Caddy (*.t.example.com, wildcard TLS)
                     │ http
                     ▼
               tamsyam-server ──WebSocket + yamux──> ts (CLI) ──> localhost:8080
```

## Возможности

- **HTTP-туннели** на случайном или своём поддомене; свой поддомен закрепляется за токеном. Работают WebSocket, SSE и HMR. В терминале печатается живой лог запросов, при выходе — сводка.
- **Туннели с паролем** (`--auth user:pass`) и **раздача папки** (`ts http ./dist`, с поддержкой SPA).
- **Конфиг и служба:** несколько туннелей из `ts.yml` командой `ts start` и `ts service install`, чтобы они поднимались после перезагрузки.
- **TCP-туннели** (`ts tcp 5432`): сервер открывает публичный порт из диапазона `TAMSYAM_TCP_PORTS` и отдаёт адрес `tcp://t.example.com:20017`. Подходит для Postgres, Redis, SSH, RDP и любого TCP. Порт, заданный через `-p`, закрепляется за токеном. TCP идёт мимо Caddy, поэтому диапазон нужно пробросить на роутере.
- **IP-фильтр** (`--allow 203.0.113.0/24,198.51.100.7`) для HTTP и TCP.
- **Инспектор запросов:** последние 200 запросов туннеля с заголовками и телами (до 32 КБ, gzip распаковывается, JSON форматируется). Запрос можно повторить или скопировать как curl.
- **Статистика:** живые счётчики (rpm, p50/p90, 4xx/5xx, соединения, трафик) и история за 30 дней: графики по статусам и трафику, популярные пути, клиенты по IP.
- **Админка:** туннели, токены, закреплённые порты; светлая и тёмная тема.
- **Доступ:** токены клиентов хранятся только в виде sha256-хэша. Админка закрыта паролем, из домашней сети можно пускать без пароля (`TAMSYAM_PRIVATE_NETWORKS`). После 10 неудачных попыток входа за 15 минут IP блокируется.

В инспекторе видны все заголовки, включая `Authorization` и cookies, поэтому админка должна быть доступна только вам.

## Установка клиента

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.ps1 | iex
```

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/Mi113r/tamsyam/main/install.sh | sh
```

Скрипт скачивает последний релиз, сверяет SHA-256 с `checksums.txt` и кладёт `ts` в `%LOCALAPPDATA%\Programs\tamsyam` (Windows) или `~/.local/bin`. Архивы есть на странице [Releases](https://github.com/Mi113r/tamsyam/releases). Через Go: `go install github.com/Mi113r/tamsyam/cmd/ts@latest`.

На Linux с пакетом `moreutils` уже есть утилита `ts`: положите наш бинарник раньше в `PATH` или переименуйте его, например в `tsm`.

## Использование

```bash
ts login https://t.example.com tsm_...
ts http 8080              # случайный поддомен
ts http 3000 -s myapp     # https://myapp.t.example.com
ts http 192.168.1.5:80    # любой host:port
ts http 8080 --allow 203.0.113.0/24   # только с этих IP
ts http 8080 --auth me:secret         # вход по паролю
ts http ./dist            # раздать папку

ts tcp 5432               # tcp://t.example.com:<случайный порт>
ts tcp 22 -p 20022        # фиксированный порт, закрепляется за токеном
ts tcp 5432 -q            # без лога соединений
```

Папка вместо порта раздаётся как статические файлы; неизвестные пути без расширения отдают `index.html`. С `--auth` сервер спрашивает пароль и убирает его из запроса до того, как тот попадёт в ваше приложение.

При обрыве клиент переподключается и сохраняет тот же адрес. Поддомен или порт, запрошенный явно (`-s`, `-p`), закрепляется за токеном: другой токен не займёт его, пока вы не освободите адрес в админке.

### Несколько туннелей: ts.yml

```yaml
# server и token по умолчанию берутся из ts login
tunnels:
  api:
    addr: 8080
    subdomain: myapi
    auth: me:secret
  site:
    addr: ./dist            # относительно этого файла
  db:
    proto: tcp
    addr: 5432
    port: 20022
    allow: [203.0.113.0/24]
```

```bash
ts start                  # все туннели из ./ts.yml
ts start api site         # только эти
ts start --config ~/tunnels.yml
```

### Запуск как служба

```bash
sudo ts service install --config /path/to/ts.yml   # systemd / launchd; на Windows — PowerShell от администратора
ts service install --user                           # Linux/macOS, без root
ts service status | stop | start | restart | uninstall
```

Служба запускает `ts start` с этим конфигом и сохранённым логином, стартует при загрузке и пишет лог в `ts.log` рядом с конфигом. С `--user` на Linux выполните `loginctl enable-linger`, чтобы служба запускалась без входа в систему.

## Развёртывание сервера

Нужны: домен в Cloudflare, сервер с белым IP (или проброшенными портами 80/443) и API-токен Cloudflare с правом `Zone:DNS:Edit`. Wildcard-сертификат выпускается через DNS-01, поэтому порт 80 для ACME не обязателен.

**DNS.** Записи `A t` и `A *.t` на белый IP, режим **DNS only** (серое облако). Universal SSL Cloudflare не покрывает поддомены второго уровня, а прокси Cloudflare обрывал бы долгие соединения.

### Docker Compose (проще всего)

Два образа: `ghcr.io/mi113r/tamsyam-server` и `ghcr.io/mi113r/tamsyam-caddy` (Caddy с модулем Cloudflare и готовым конфигом), для amd64 и arm64.

```bash
mkdir tamsyam && cd tamsyam
curl -fsSLO https://raw.githubusercontent.com/Mi113r/tamsyam/main/deploy/docker/compose.yml
curl -fsSL -o .env https://raw.githubusercontent.com/Mi113r/tamsyam/main/deploy/docker/.env.example
# заполните .env: домен, пароль админки, токен Cloudflare
docker compose up -d
```

Откройте `https://t.example.com`, создайте токен в разделе «Токены» и подключите клиент. Для TCP-туннелей пробросьте на роутере диапазон `TAMSYAM_TCP_PORTS` (по умолчанию `20000-20019`). Обновление: `docker compose pull && docker compose up -d`.

### Бинарник + systemd (если Caddy уже есть)

Пошаговая инструкция с проверками и откатом, рассчитанная на агента: [DEPLOY.md](DEPLOY.md). Кратко:

1. Соберите Caddy с модулем `caddy-dns/cloudflare` и добавьте блок из [deploy/Caddyfile](deploy/Caddyfile).
2. Установите сервер:
   ```bash
   useradd -r -s /usr/sbin/nologin tamsyam
   mkdir -p /var/lib/tamsyam /etc/tamsyam && chown tamsyam: /var/lib/tamsyam
   cp tamsyam-server /usr/local/bin/
   cp deploy/server.env.example /etc/tamsyam/server.env   # отредактировать
   cp deploy/tamsyam-server.service /etc/systemd/system/
   systemctl enable --now tamsyam-server
   ```
3. Откройте `https://t.example.com` (пароль из `TAMSYAM_ADMIN_PASSWORD`) и создайте токен.

### IP клиентов, LAN и VPN

Реальный IP клиента сервер берёт из `X-Forwarded-For`, но только от доверенного прокси: loopback или адреса из `TAMSYAM_TRUSTED_PROXIES` (в Docker Compose это уже настроено). Заголовок читается справа налево, поэтому подставленные клиентом значения не учитываются. Если Caddy стоит на другом хосте, укажите его IP в `TAMSYAM_TRUSTED_PROXIES`, иначе все запросы будут выглядеть как пришедшие от Caddy.

`TAMSYAM_PRIVATE_NETWORKS` (CIDR/IP через запятую) открывает админку без пароля из этих сетей. Роутер может выпускать VPN-клиентов под своим LAN-адресом, поэтому сначала измерьте, какие адреса видит сервер. Порядок действий и проверки описаны в [AGENTS.md](AGENTS.md).

Обслуживание работающего сервера (обновление, пароли, токены, сети) описано в [AGENTS.md](AGENTS.md). Все переменные с пояснениями перечислены в [deploy/server.env.example](deploy/server.env.example).

## Разработка

```bash
go build -o bin/ ./cmd/...
go test ./...
TAMSYAM_DOMAIN=localhost TAMSYAM_ADMIN_PASSWORD=dev TAMSYAM_PUBLIC_SCHEME=http TAMSYAM_PUBLIC_PORT=8090 bin/tamsyam-server
# админка: http://localhost:8090, туннели: http://<sub>.localhost:8090
ts http 8080 --server http://localhost:8090 --token tsm_...
```

Релиз создаётся пушем тега `vX.Y.Z`. GitHub Actions соберёт клиент и сервер через GoReleaser и опубликует Docker-образы в GHCR.

## Идеи для развития

- Инспектор: редактирование запроса перед повтором, live-обновление, фильтры.
- Перехват вебхуков: запросы сохраняются, пока клиент выключен, и повторяются позже.
- Туннель без клиента (`ssh -R`), SSH на машину без sshd.
- Свои домены для туннелей, правила трафика (заголовки, CORS, rate limit), OAuth перед туннелем.
- Лимиты на токен, география клиентов по GeoLite2.
- Пользователи и OAuth в админке вместо одного пароля, метрики Prometheus.

## Лицензия

[MIT](LICENSE)
