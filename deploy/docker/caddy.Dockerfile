# Caddy with the Cloudflare DNS module (wildcard certificates via DNS-01) and
# the tamsyam site config baked in. Cross-compiled like the server image.
FROM --platform=$BUILDPLATFORM caddy:2.11-builder AS build
ARG TARGETOS TARGETARCH
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH xcaddy build --with github.com/caddy-dns/cloudflare@v0.2.4

FROM caddy:2.11
COPY --from=build /usr/bin/caddy /usr/bin/caddy
COPY deploy/docker/Caddyfile /etc/caddy/Caddyfile
