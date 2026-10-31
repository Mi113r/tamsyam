# Деплой tamsyam — инструкция для агента

Ты разворачиваешь сервер туннелей **tamsyam** на инфраструктуре пользователя: Proxmox, белый IP, уже работающий Caddy, DNS в Cloudflare. В конце пользователь должен открыть админку по `https://<BASE_DOMAIN>` и запустить `ts http 8080` у себя на машине, получив рабочий `https://<sub>.<BASE_DOMAIN>`.

Код лежит в этом репозитории. Как всё устроено, описано в [README.md](README.md). Готовые файлы для деплоя лежат в [deploy/](deploy/).

## Правила

- **Ничего не перезаписывай целиком.** Caddyfile, DNS-зона и systemd на хостах уже используются другими сервисами. Только добавляй свои блоки и записи, а перед изменением делай резервную копию.
- **Спроси пользователя перед** заменой бинарника Caddy, открытием портов в фаерволе, созданием нового LXC/VM и удалением чего-либо.
- **Секреты** (Cloudflare API token, пароль админки, токены клиентов) не выводи в чат, не пиши в аргументы команд и не оставляй в истории shell. Храни их в файлах с правами `600`. Пользователю скажи, *где* лежит секрет, а не сам секрет. Исключение — пароль админки и первый клиентский токен: их пользователю нужно увидеть один раз в финальном отчёте.
- После каждого шага выполняй его проверку. Не переходи дальше, пока проверка не прошла.
- Если что-то идёт не так, как описано, остановись и опиши пользователю фактическое состояние. Не импровизируй с чужой инфраструктурой.

## Шаг 0. Собери входные данные

Узнай у пользователя то, чего нет в контексте. Не угадывай.

| Переменная | Что это | Пример |
|---|---|---|
| `BASE_DOMAIN` | Домен сервиса. Админка будет на нём, туннели на `*.BASE_DOMAIN` | `t.example.com` |
| `ZONE` | Зона в Cloudflare | `example.com` |
| `PUBLIC_IP` | Белый IP, на который проброшены 80/443 к Caddy | `203.0.113.10` |
| `CADDY_SSH` | SSH-доступ к хосту/контейнеру с Caddy | `root@10.0.0.5` |
| `SERVER_SSH` | Куда ставить tamsyam-server: отдельный LXC или тот же хост, что Caddy | `root@10.0.0.20` |
| `SERVER_ADDR` | Адрес, по которому Caddy достучится до tamsyam-server | `10.0.0.20:8090` или `127.0.0.1:8090` |
| `CF_API_TOKEN` | Токен Cloudflare с правами `Zone → DNS → Edit` и `Zone → Zone → Read` на `ZONE` | — |
| `TCP_PORTS` | Диапазон публичных портов для TCP-туннелей (`ts tcp`). Пусто — TCP выключен | `20000-20099` |
| `SERVER_LAN_IP` | LAN-адрес `SERVER_SSH`, на который роутер пробросит `TCP_PORTS` | `10.0.0.20` |

Спроси, нужны ли TCP-туннели. Если да, диапазон портов и проброс на роутере согласуй с пользователем: роутер обычно настраивает он сам.

Уточни ещё одно: как сейчас Caddy получает сертификаты. Если в нём уже настроен Cloudflare DNS-01, шаг 5 станет проще.

`ADMIN_PASSWORD` сгенерируй сам: `openssl rand -base64 24`.

Почему именно wildcard по DNS-01: у каждого туннеля свой поддомен, а выпускать сертификат Let's Encrypt на каждый не позволят лимиты (50 сертификатов в неделю). Wildcard `*.BASE_DOMAIN` можно получить только через DNS-01, поэтому Caddy нужен модуль Cloudflare.

## Шаг 1. Предварительные проверки

1. Локально, в корне репозитория:
   ```bash
   go vet ./... && go test ./... -count=1
   ```
   Все тесты должны пройти.
2. Проверь SSH-доступ к `CADDY_SSH` и `SERVER_SSH`. На обоих выполни `uname -m` и `cat /etc/os-release`. Ожидается Linux с systemd; архитектура `x86_64` или `aarch64`.
3. На хосте Caddy:
   ```bash
   caddy version
   caddy list-modules | grep dns.providers.cloudflare
   systemctl cat caddy | grep -E 'ExecStart|EnvironmentFile|Environment'
   ```
   Найди действующий Caddyfile: обычно `/etc/caddy/Caddyfile`, но точный путь смотри в `ExecStart`. Запомни, есть ли модуль cloudflare.
4. С любой внешней машины или по ответу пользователя убедись, что `PUBLIC_IP:80` и `:443` доходят до Caddy.

## Шаг 2. Сборка

Локально. `GOARCH` возьми из шага 1: `amd64` для x86_64, `arm64` для aarch64.

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/linux/tamsyam-server ./cmd/tamsyam-server
```

Клиент собери под ОС пользователя: на Windows это `go build -o bin/ ./cmd/ts`, для macOS или Linux укажи соответствующие `GOOS`/`GOARCH`.

## Шаг 3. Установка tamsyam-server

Если `SERVER_SSH` — новый LXC, его создание сначала согласуй с пользователем. Достаточно Debian 12, 1 vCPU, 256–512 МБ RAM и 2 ГБ диска.

На `SERVER_SSH`:

```bash
id tamsyam 2>/dev/null || useradd -r -s /usr/sbin/nologin tamsyam
install -d -o tamsyam -g tamsyam -m 750 /var/lib/tamsyam
install -d -m 755 /etc/tamsyam
```

Скопируй файлы, например через `scp`:
- `bin/linux/tamsyam-server` → `/usr/local/bin/tamsyam-server` (`chmod 755`);
- `deploy/tamsyam-server.service` → `/etc/systemd/system/tamsyam-server.service`.

Создай `/etc/tamsyam/server.env` с правами `600`, владелец `root`. За образец возьми [deploy/server.env.example](deploy/server.env.example):

```ini
TAMSYAM_DOMAIN=<BASE_DOMAIN>
TAMSYAM_ADMIN_PASSWORD=<ADMIN_PASSWORD>
TAMSYAM_LISTEN=<127.0.0.1:8090 если Caddy на этом же хосте, иначе 0.0.0.0:8090>
TAMSYAM_DB=/var/lib/tamsyam/tamsyam.db
TAMSYAM_PUBLIC_SCHEME=https
# Только если нужны TCP-туннели:
TAMSYAM_TCP_PORTS=<TCP_PORTS>
TAMSYAM_TCP_HOST=<BASE_DOMAIN>
# Опционально: сети (CIDR/IP, через запятую), где админка работает без пароля
# (доверенная LAN). VPN-клиенты и все остальные вводят пароль как обычно.
#TAMSYAM_PRIVATE_NETWORKS=10.1.1.0/24
# Только если Caddy на ДРУГОМ хосте: его IP, иначе сервер видит все запросы от Caddy
# (не работают --allow, IP в статистике и беспарольный вход из LAN).
TAMSYAM_TRUSTED_PROXIES=<IP хоста Caddy>
```

Запусти:

```bash
systemctl daemon-reload
systemctl enable --now tamsyam-server
```

**Проверка.** Сервер различает запросы по заголовку `Host`, поэтому его нужно передать явно:

```bash
systemctl is-active tamsyam-server
curl -s -H "Host: <BASE_DOMAIN>" http://127.0.0.1:8090/healthz
# должен ответить: ok
```

**Если сервис не стартует в LXC** и в `journalctl -u tamsyam-server` есть `Failed to set up mount namespacing` или `status=226/NAMESPACE`, значит, непривилегированный контейнер не поддерживает изоляцию из юнита. Есть два пути: включить в Proxmox для контейнера `nesting=1` (с согласия пользователя) или закомментировать строки `ProtectSystem`, `ProtectHome`, `PrivateTmp` и `ReadWritePaths` в юните. После этого снова выполни `daemon-reload` и `restart`.

## Шаг 4. Сеть между Caddy и сервером

Пропусти этот шаг, если Caddy и сервер на одном хосте.

С хоста Caddy проверь доступ:

```bash
curl -s -H "Host: <BASE_DOMAIN>" http://<SERVER_ADDR>/healthz   # ok
```

Если на `SERVER_SSH` есть фаервол, порт 8090 должен быть открыт только для IP хоста Caddy. Правило согласуй с пользователем. Снаружи порт 8090 открыт быть не должен.

### 4.1. TCP-порты (только если нужны TCP-туннели)

TCP-трафик идёт **мимо Caddy**, напрямую на `tamsyam-server`. Поэтому:
- На роутере нужен проброс `PUBLIC_IP:<TCP_PORTS>` → `SERVER_LAN_IP:<TCP_PORTS>` (TCP, тот же диапазон портов). Обычно это делает пользователь; дай ему точные значения.
- Если на `SERVER_SSH` есть фаервол, открой в нём диапазон `TCP_PORTS` (согласуй правило с пользователем).
- Отдельная DNS-запись не нужна: адреса имеют вид `<BASE_DOMAIN>:<порт>`, а `BASE_DOMAIN` уже указывает на `PUBLIC_IP` (шаг 6).

Порты из диапазона сервер открывает только на время работы туннеля, остальное время они закрыты.

## Шаг 5. Caddy

### 5.1. Модуль Cloudflare

Если `caddy list-modules` уже показал `dns.providers.cloudflare`, переходи к 5.2.

Если модуля нет, Caddy придётся заменить сборкой с этим модулем. **Сначала спроси пользователя.** Варианты:
- скачать готовую сборку `https://caddyserver.com/api/download?os=linux&arch=amd64&p=github.com/caddy-dns/cloudflare`;
- собрать через `xcaddy build --with github.com/caddy-dns/cloudflare`.

Порядок замены:
1. Сделай бэкап текущего бинарника: `cp $(which caddy) $(which caddy).bak`.
2. Положи новый бинарник на то же место.
3. Проверь `caddy version` и `caddy list-modules | grep cloudflare`.
4. Если Caddy установлен из apt-пакета, предупреди пользователя: `apt upgrade` вернёт стандартную сборку. Предложи `apt-mark hold caddy`.

### 5.2. Токен Cloudflare для Caddy

Положи токен в файл `/etc/caddy/cloudflare.env` (права `600`, владелец `root`):

```ini
CF_API_TOKEN=<CF_API_TOKEN>
```

Подключи файл через override, не трогая основной юнит:

```bash
mkdir -p /etc/systemd/system/caddy.service.d
cat > /etc/systemd/system/caddy.service.d/tamsyam.conf <<'EOF'
[Service]
EnvironmentFile=/etc/caddy/cloudflare.env
EOF
systemctl daemon-reload
```

Если токен для Cloudflare в Caddy уже настроен, используй существующий механизм и не дублируй его.

### 5.3. Блок сайта

Сделай бэкап Caddyfile: `cp Caddyfile Caddyfile.bak.$(date +%s)`. Затем **допиши** в конец блок по образцу [deploy/Caddyfile](deploy/Caddyfile), подставив свои значения:

```caddy
<BASE_DOMAIN>, *.<BASE_DOMAIN> {
	tls {
		dns cloudflare {env.CF_API_TOKEN}
	}
	reverse_proxy <SERVER_ADDR> {
		flush_interval -1
	}
}
```

Важно:
- **Не добавляй** `header_up Host ...`. Caddy по умолчанию передаёт исходный `Host`, и сервер маршрутизирует именно по нему.
- Если в Caddyfile уже есть глобальная опция `acme_dns cloudflare ...`, блок `tls { ... }` можно опустить.
- Если `*.<ZONE>` уже обслуживается другим блоком, сообщи пользователю о возможном конфликте до reload.

Примени изменения:

```bash
caddy validate --config <путь к Caddyfile> --adapter caddyfile
systemctl restart caddy    # restart, а не reload: нужно подхватить новый EnvironmentFile
```

Если valid не прошёл, верни бэкап и сообщи пользователю. Сертификаты Caddy выпустит после появления DNS-записей (шаг 6). Следи за этим так: `journalctl -u caddy -f | grep -i -E 'certificate|acme|error'`.

## Шаг 6. DNS в Cloudflare

Нужны две A-записи на `PUBLIC_IP`, **обе в режиме DNS only (`proxied: false`)**:
- `<BASE_DOMAIN>`;
- `*.<BASE_DOMAIN>`.

Режим DNS only обязателен по двум причинам. Universal SSL от Cloudflare не покрывает поддомены второго уровня. А прокси Cloudflare обрывает долгие соединения: WebSocket, SSE, HMR.

Через API. Токен читай из файла, а не из аргумента команды:

```bash
set -a; . /etc/caddy/cloudflare.env; set +a   # или локальный файл с правами 600
API=https://api.cloudflare.com/client/v4
AUTH="Authorization: Bearer $CF_API_TOKEN"

ZONE_ID=$(curl -s -H "$AUTH" "$API/zones?name=<ZONE>" | jq -r '.result[0].id')

# Сначала посмотри, нет ли уже таких записей:
curl -s -H "$AUTH" "$API/zones/$ZONE_ID/dns_records?name=<BASE_DOMAIN>" | jq '.result[] | {id,type,content,proxied}'
curl -s -H "$AUTH" "$API/zones/$ZONE_ID/dns_records?name=*.<BASE_DOMAIN>" | jq '.result[] | {id,type,content,proxied}'
```

- Записи нет — создай:
  ```bash
  for NAME in "<BASE_DOMAIN>" "*.<BASE_DOMAIN>"; do
    curl -s -X POST -H "$AUTH" -H "Content-Type: application/json" "$API/zones/$ZONE_ID/dns_records" \
      --data "{\"type\":\"A\",\"name\":\"$NAME\",\"content\":\"<PUBLIC_IP>\",\"ttl\":300,\"proxied\":false}" | jq '.success'
  done
  ```
- Запись есть и указывает куда-то ещё или проксируется — **спроси пользователя**, прежде чем менять (`PATCH .../dns_records/<id>`).

**Проверка:**

```bash
dig +short <BASE_DOMAIN> @1.1.1.1
dig +short random123.<BASE_DOMAIN> @1.1.1.1
# оба должны вернуть PUBLIC_IP
```

Затем дождись в логах Caddy выпуска сертификатов для `<BASE_DOMAIN>` и `*.<BASE_DOMAIN>`. Обычно это занимает 1–3 минуты.

## Шаг 7. Проверка от начала до конца

```bash
curl -s https://<BASE_DOMAIN>/healthz                                   # ok
curl -s -o /dev/null -w '%{http_code}\n' https://<BASE_DOMAIN>/api/info  # 401
curl -s -o /dev/null -w '%{http_code}\n' https://nope.<BASE_DOMAIN>/     # 404 (страница «Tunnel not found»)
```

Создай первый клиентский токен через API. Пароль передай через файл или переменную окружения, а не аргументом команды:

```bash
curl -s -u "admin:$ADMIN_PASSWORD" -H 'Content-Type: application/json' \
  --data '{"name":"<имя машины пользователя>"}' https://<BASE_DOMAIN>/api/tokens
# поле "secret" — токен клиента, он показывается один раз
```

Проверь туннель. Это можно сделать с любой машины с клиентом, хоть с самого сервера:

```bash
python3 -m http.server 18080 &          # любой локальный сервис
./ts http 18080 -s deploycheck --server https://<BASE_DOMAIN> --token <secret>
# в другом терминале:
curl -s -o /dev/null -w '%{http_code}\n' https://deploycheck.<BASE_DOMAIN>/   # 200
```

Если включены TCP-туннели, проверь и их. Подключаться нужно **снаружи** сети пользователя: многие роутеры не умеют NAT loopback, и изнутри проверка ложно провалится. Если внешней машины нет, скажи об этом в отчёте.

```bash
./ts tcp 18080 --server https://<BASE_DOMAIN> --token <secret>
# печатает адрес вида tcp://<BASE_DOMAIN>:<порт>; снаружи:
curl -s -o /dev/null -w '%{http_code}\n' http://<BASE_DOMAIN>:<порт>/   # 200
```

Затем открой `https://<BASE_DOMAIN>`, войди с любым логином и паролем `ADMIN_PASSWORD`. Убедись, что туннель `deploycheck` виден, в «Запросах» есть твой запрос, а в «Статистике» за 1 час появились запросы (данные сбрасываются на диск раз в 10 секунд). Останови тестовые клиенты и `http.server`.

## Шаг 8. Отчёт пользователю

Сообщи:
- адрес админки и пароль (один раз). Скажи, что пароль хранится в `/etc/tamsyam/server.env` на `SERVER_SSH`;
- команды для его машины:
  ```bash
  ts login https://<BASE_DOMAIN> <secret>
  ts http 8080
  ts tcp 5432        # если включены TCP-туннели
  ```
- для TCP: какой диапазон портов нужно пробросить на роутере (если пользователь ещё не сделал);
- что изменено на каждом хосте: файлы, юниты, DNS-записи, бэкапы;
- всё, что пропущено или не удалось проверить.

## Обновление (последующие деплои)

Для сервера, который уже работает, используй [AGENTS.md](AGENTS.md): там описано обновление из релиза с проверкой хэша, а также настройка доступа из LAN и VPN, паролей и токенов. Ниже вариант со сборкой из исходников.

1. Собери новый бинарник (шаг 2).
2. Скопируй его на `SERVER_SSH` во временный файл. Затем выполни:
   ```bash
   install -m 755 /tmp/tamsyam-server /usr/local/bin/tamsyam-server
   systemctl restart tamsyam-server
   ```
3. Проверь `/healthz`.

Активные туннели при рестарте оборвутся, клиенты переподключатся сами за несколько секунд и сохранят свои адреса. История инспектора хранится в памяти и при рестарте пропадёт. Токены лежат в SQLite и сохраняются.

## Откат

- **Caddy:** верни `Caddyfile.bak.*`. Если менялся бинарник, верни `caddy.bak`. Удали `/etc/systemd/system/caddy.service.d/tamsyam.conf`, затем выполни `systemctl daemon-reload && systemctl restart caddy`.
- **Сервер:**
  ```bash
  systemctl disable --now tamsyam-server
  rm /etc/systemd/system/tamsyam-server.service /usr/local/bin/tamsyam-server
  ```
  Каталоги `/etc/tamsyam` и `/var/lib/tamsyam` удаляй только с согласия пользователя: там токены.
- **DNS:** удали две созданные записи по их `id` (`DELETE $API/zones/$ZONE_ID/dns_records/<id>`).
