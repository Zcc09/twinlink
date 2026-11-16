# syntax=docker/dockerfile:1

# ---- build stage -------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so the module cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w" -o /out/twinlink-relay ./cmd/twinlink-relay

# ---- runtime stage -----------------------------------------------------------
FROM alpine:3.22

# iptables + iproute2 are what --setup-nat drives; ca-certificates keeps the
# image usable for debugging with wget/curl.
RUN apk add --no-cache iptables iproute2 ca-certificates

COPY --from=build /out/twinlink-relay /usr/local/bin/twinlink-relay

# 51821/udp: the tunnel. 51822/tcp: the control API (unauthenticated).
EXPOSE 51821/udp
EXPOSE 51822/tcp

# The container needs NET_ADMIN and /dev/net/tun from compose.yaml; a TUN
# device cannot be created without them, so this image stays root.
ENTRYPOINT ["/usr/local/bin/twinlink-relay"]
CMD ["--listen", "0.0.0.0:51821", "--http", "0.0.0.0:51822", "--log-level", "info"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O - http://127.0.0.1:51822/healthz || exit 1
