# tamsyam-server image. Built for linux/amd64 and linux/arm64 by cross-compiling
# on the build platform, so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/Mi113r/tamsyam/internal/protocol.Version=$VERSION" \
      -o /out/tamsyam-server ./cmd/tamsyam-server \
 && mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tamsyam-server /usr/local/bin/tamsyam-server
# Named volumes inherit this ownership, so the nonroot user can write the DB.
COPY --from=build --chown=65532:65532 /out/data /data
ENV TAMSYAM_LISTEN=0.0.0.0:8090 \
    TAMSYAM_DB=/data/tamsyam.db
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/tamsyam-server"]
