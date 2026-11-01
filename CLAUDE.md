# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go Discord bot that watches messages for social-media links (TikTok, Instagram, X/Twitter, Reddit, Bluesky, YouTube Shorts, Twitch clips), downloads the underlying media, re-hosts it on a storage destination, and replies with a Components V2 message (embed text + media gallery) so the video plays inline in Discord. It also hides the original message's embeds.

## Commands

Go version is managed by `mise` (`mise.toml`); `go.mod` targets Go 1.27.

```sh
go build ./...          # build
go vet ./...            # lint (no other linter configured)
go run .                # run locally (needs a Chrome/Chromium on PATH, or CDP_URL/ROD_URL set)
docker compose up --build          # full dev stack: app + cobalt + rod (headless Chrome) + rclone webdav
docker compose watch               # same, rebuilding the app on file changes
```

There are no tests in the repo yet.

**Without `DISCORD_TOKEN`**, the app does not connect to Discord; instead it serves a debug UI on `:8081` (`bot/simpleserver.go`) where you can paste a URL and run a reupload end-to-end. This is the main way to exercise extractors during development.

## Configuration (env vars, parsed in `main.go`)

All backends are configured as URLs whose **scheme selects the implementation** and whose query string carries options:

- `DESTINATION` — `fs://<path>?server=host:port` (local dir, optionally served over HTTP), `b2://...` (Backblaze B2), `rclone+webdav://host:port`. Dispatch in `preview.NewDestination`.
- `EXTRACTORS` (comma-separated) and/or `EXTRACTOR_1`…`EXTRACTOR_10` — currently only `cobalt://host:port?insecure=1&key=...` (`insecure` → http instead of https). Dispatch in `preview.NewExtractor`.
- `PUBLIC_URL` — base URL prepended to stored filenames to form the links posted to Discord.
- `DISCORD_TOKEN` — enables bot mode.
- `CDP_URL` — connect to a shared CDP endpoint such as browserless (`ws(s)://` or `http(s)://`, query like `?token=` preserved). Takes precedence over `ROD_URL`. Opens a new connection (= new browserless session) per page instead of holding one browser open.
- `ROD_URL` — connect to a remote rod/Chrome manager instead of launching a local browser.
- `OTEL_EXPORTER_OTLP_ENDPOINT` / `_PROTOCOL` (`grpc` default, or `http/protobuf`) / `_HEADERS` — tracing; a no-op tracer is used if the endpoint is unset.

## Architecture

```
bot (Discord handler | debug HTTP server)
  └─> preview.Reuploader.Reupload(url)
        ├─ clean URL params → mediaID = first 12 hex chars of sha256(cleanURL)
        ├─ fast path: Destination.Download("<mediaID>.json") manifest exists → return permalinks
        └─ slow path: Extractors (first that IsSupported and succeeds) → remote URLs
                      → download into memory (≤500MB, http(s):// or file://)
                      → sniff content type (mp4/jpeg/png only) → Destination.Upload("<mediaID>[-n].<ext>")
                      → write manifest JSON → return PUBLIC_URL/<file> permalinks
```

Key points that span multiple files:

- **`preview.Extractor`** (`IsSupported`, `Extract`) and **`preview.Destination`** (`Upload`, `Download`, `Close`, `String`) in `preview/preview.go` are the two extension points. Each implementation lives in `preview/extractor_*.go` / `preview/destination_*.go` and asserts the interface with `var _ Extractor = (*X)(nil)`.
- **Extractor order matters.** URL-configured extractors (cobalt) come first; the hardcoded browser-scraping fallbacks (`FastDL`, `SSSTik`, `ReelsVideo`) are appended in `main.go`. `Reuploader.extract` tries each supporting extractor in order and joins errors if all fail.
- **URL matching** uses `simpleURLMatch` (`preview/helpers.go`): strips `https://` and `www.`, then glob-matches with `tidwall/match` patterns like `"instagram.com/reel/*"`.
- **Browser-scraping extractors** drive a third-party download site via go-rod using the helpers in `browser/browser.go`: `browser.TryChan` gives a fresh page and converts rod's `Must*` panics into errors; `BlockThirdParty` blocks non-first-party and non-essential requests. Follow the existing extractors' shape (`page.Timeout`, `page.Race()` between success and error elements).
- Extractors may return `file://` URLs (e.g. for locally produced files); the Docker image includes `ffmpeg` and `yt-dlp` for this purpose.
- Destinations must reject filenames containing path separators (`validateSimpleFilename`); the manifest's not-found error must satisfy `errors.Is(err, fs.ErrNotExist)` for the cache fast path to work.
- **HTTP calls** go through `httpGet`/`httpDo`/`JSONRequest[V, E]` in `preview/helpers.go`, which share a cookie jar, a browser User-Agent, a 30s timeout, and emit trace spans.
- **Tracing**: package-level `tracer = otel.Tracer(...)`; spans are closed with `defer tr.End(span, &err)` to record the named return error. The `tr` package initializes the provider in `init()`.
- **Discord bot** (`bot/discordbot.go`): grabs the first `https://` URL in a message, waits up to 3s for Discord's own embed (used for the reply's title/description text), then replies — only using a message reference if other messages have arrived in the channel since.
- **"Video Preview" message command** (`bot/discordcommand.go`): a right-click > Apps context menu command, registered globally on ready. It defers the interaction, reuploads the target message's first link, and edits the deferred response via a raw PATCH (discordgo's `WebhookEdit` can't set the Components V2 flag). On failure the public response is deleted and replaced with an ephemeral error.
