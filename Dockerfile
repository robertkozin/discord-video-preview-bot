FROM golang:alpine AS builder

WORKDIR /app
WORKDIR /build

# yt-dlp from PyPI (Alpine's package lags behind site changes, and the
# extractor runs with --no-update) with:
# - [default], which includes yt-dlp-ejs, plus deno to run it: without a JS
#   runtime yt-dlp only uses YouTube's visionos client, the same one the
#   youtube extractor uses, so it would fail together with it
# - the bgutil PO token plugin; its server is the bgutil-ytdlp-pot-provider
#   container, keep both on the same version
# PyPI's yt-dlp metadata changes with each release, so ADDing it invalidates
# this layer and a plain rebuild picks up the new version.
ADD https://pypi.org/pypi/yt-dlp/json /tmp/yt-dlp-release.json
RUN apk add --no-cache ffmpeg python3 ca-certificates deno \
    && python3 -m venv /opt/yt-dlp \
    && /opt/yt-dlp/bin/pip install --no-cache-dir -U "yt-dlp[default]" "bgutil-ytdlp-pot-provider==2.0.2" \
    && ln -s /opt/yt-dlp/bin/yt-dlp /usr/local/bin/yt-dlp \
    && yt-dlp --version && deno --version

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
