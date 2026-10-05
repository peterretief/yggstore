# The S3 gateway with its own Yggdrasil, built from source so the host needs
# only Docker. Build from the repository root:
#   docker build -f deploy/gateway/Dockerfile .
# scripts/gateway-docker.sh does this for you.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -ldflags "-s -w" -o /out/yggstore ./cmd/yggstore && \
    GOBIN=/out go install -ldflags "-s -w" \
      github.com/yggdrasil-network/yggdrasil-go/cmd/yggdrasil@v0.5.12 \
      github.com/yggdrasil-network/yggdrasil-go/cmd/yggdrasilctl@v0.5.12

FROM alpine:3.22
RUN apk add --no-cache jq ca-certificates
COPY --from=build /out/ /usr/local/bin/
COPY deploy/gateway/entrypoint.sh /entrypoint.sh
VOLUME /data
ENTRYPOINT ["/entrypoint.sh"]
