FROM golang:alpine AS builder

WORKDIR /app
WORKDIR /build

# Alpine's yt-dlp package lags behind site changes and the extractor runs with
# --no-update, so install the latest release (a python zipapp). Rebuild with
# --no-cache to pick up a newer one.
RUN apk add --no-cache ffmpeg python3 ca-certificates \
    && wget -q -O /usr/local/bin/yt-dlp https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp \
    && yt-dlp --version

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=bind,source=go.sum,target=go.sum \
    --mount=type=bind,source=go.mod,target=go.mod \
    go mod download -x

ENV GOCACHE=/root/.cache/go-build
RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target="/root/.cache/go-build" \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 go build -o /app/main

WORKDIR /app

CMD ["./main"]
