# A yggstore box in a container: its own Yggdrasil, node, dashboard, hub and
# Syncthing, as on a Raspberry Pi. See docs/docker-box.md.
#   docker compose -f docker/box/compose.yml up -d --build
FROM golang:1.24-alpine AS build
ARG VERSION=docker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd cmd
COPY internal internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /yggstore ./cmd/yggstore

FROM alpine:3.22
RUN apk add --no-cache yggdrasil syncthing jq iproute2 tzdata || true; \
    command -v yggdrasil && command -v syncthing && command -v jq
COPY --from=build /yggstore /usr/local/bin/yggstore
COPY docker/box/box.sh /usr/local/bin/box
ENV HOME=/data TZ=Africa/Johannesburg TERM=linux
VOLUME /data
ENTRYPOINT ["box"]
CMD ["run"]
