# syntax=docker/dockerfile:1

########## build ##########
FROM golang:1.26-alpine3.24 AS build

RUN apk add --no-cache build-base pkgconf lame-dev opus-dev opusfile-dev patch

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# Patch sendspin-go v1.8.2 in a private module copy. Its WebSocket reader must
# never block on the audio queue, otherwise server/time replies cannot reach
# the clock-sync burst and the player eventually becomes unreachable.
RUN sdk="$(go list -m -f '{{.Dir}}' github.com/Sendspin/sendspin-go)" \
 && cp -a "$sdk" /tmp/sendspin-go \
 && chmod -R u+w /tmp/sendspin-go
COPY patches/sendspin-go-backpressure.patch /tmp/sendspin-go-backpressure.patch
RUN cd /tmp/sendspin-go \
 && patch -p1 < /tmp/sendspin-go-backpressure.patch \
 && go test ./pkg/protocol

COPY src/ ./src/
RUN go mod edit -replace github.com/Sendspin/sendspin-go=/tmp/sendspin-go
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags "-s -w" -o /out/sendspin-streamer ./src

########## runtime ##########
FROM alpine:3.24

RUN apk add --no-cache lame-libs opus opusfile ca-certificates tzdata \
 && adduser -D -u 10001 sendspin

COPY --from=build /out/sendspin-streamer /usr/local/bin/sendspin-streamer

USER sendspin
EXPOSE 8000

ENV HTTP_PORT=8000 \
    MP3_BITRATE=192 \
    BUFFER_MS=800 \
    VOLUME=100

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s \
  CMD wget -qO- "http://127.0.0.1:${HTTP_PORT}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/sendspin-streamer"]
