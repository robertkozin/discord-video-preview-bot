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
- `EXTRACTORS` (comma-separated) and/or `EXTRACTOR_1`…`EXTRACTOR_10` — `cobalt://host:port?insecure=1&key=...` (`insecure` → http instead of https), `brightdata://?key=<api key>`, `ytdlp://?proxy=<url-encoded proxy url>` (proxy optional), `fxtwitter://[host:port?insecure=1]` (X only; host optional, defaults to api.fxtwitter.com), `tikwm://`, `reddit://` (hosted videos via the OAuth API; needs `REDDIT_CLIENT_ID`/`REDDIT_CLIENT_SECRET`/`REDDIT_REFRESH_TOKEN`), `instafix://<host>[?carousel=0&insecure=1]` (Instagram via an embed-fixer site such as `eeinstagram.com` or `vxinstagram.com`; `carousel=0` takes only the first item, for hosts like vxinstagram that answer past the end of a carousel), or the option-less browser scrapers `fastdl://`, `ssstik://`, `reelsvideo://`. Dispatch in `preview.NewExtractor`.
- `PUBLIC_URL` — base URL prepended to stored filenames to form the links posted to Discord.
- `DISCORD_TOKEN` — enables bot mode.
- `CDP_URL` — connect to a shared CDP endpoint such as browserless (`ws(s)://` or `http(s)://`, query like `?token=` preserved). Takes precedence over `ROD_URL`. Opens a new connection (= new browserless session) per page instead of holding one browser open. Connections go through a gorilla/websocket adapter (`browser/websocket.go`) because rod's built-in websocket parses close frames as JSON and panics when browserless ends a session.
- `ROD_URL` — connect to a remote rod/Chrome manager instead of launching a local browser.
- `CANARY_SOURCE` (optional, comma-separated, e.g. `reddit://` or `reddit://?domains=x.com&domains=tiktok.com&sort=hot&limit=50`) — enables ongoing testing, see below. The Reddit source needs `REDDIT_CLIENT_ID`/`REDDIT_CLIENT_SECRET`/`REDDIT_REFRESH_TOKEN`. `CANARY_INTERVAL` (default `10m`) is the gap between checks and `CANARY_REFRESH` (default `30m`) the gap between fetching new links. `HTTP_ADDR` serves the debug page (defaults to `0.0.0.0:8081` when there is no `DISCORD_TOKEN`; publish it on a private address only) and `GRAFANA_URL` links each canary check on it to its trace.
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
                      → (extractors without metadata: providers describe the post meanwhile)
                      → write manifest JSON (files + metadata) → return PUBLIC_URL/<file> permalinks
```

Key points that span multiple files:

- **`preview.Extractor`** (`IsSupported`, `Extract`) and **`preview.Destination`** (`Upload`, `Download`, `Close`, `String`) in `preview/preview.go` are the two extension points. Each implementation lives in `preview/extractor_*.go` / `preview/destination_*.go` and asserts the interface with `var _ Extractor = (*X)(nil)`.
- **Extractor order matters.** Every extractor is URL-configured, so the order is the order of `EXTRACTORS` then `EXTRACTOR_1`…, e.g. cheap HTTP APIs (`fxtwitter`, `tikwm`, `reddit`, `instafix`, `cobalt`) before slow or paid ones (`ytdlp`, `brightdata`) and the browser-scraping fallbacks (`fastdl`, `ssstik`, `reelsvideo`) last. `Reuploader.extract` tries each supporting extractor in order and joins errors if all fail. If the winning extractor's media then fails to transfer (e.g. a cobalt tunnel answering with an empty body), `Reupload` continues with the next extractor after it (`transfer_failed` span event, `preview.transfer_fallbacks`).
- **Metadata** (`preview/metadata.go`): extractors can optionally say what they know about the post (author, handle, title, description, likes/views/comments/shares, posted time) so the reply's text doesn't depend on Discord's embed. Two optional interfaces on an `Extractor`: `MediaExtractor.ExtractMedia` returns the media and metadata from one call (`fxtwitter`, `tikwm`, `instafix` from the page's meta tags, `ytdlp` via a `--print` JSON template, `brightdata` with best-guess field names that haven't been checked against a live record), and `MetadataProvider.Metadata` describes a post without extracting it (`fxtwitter`, `tikwm`, `instafix`). When the extractor that won (e.g. cobalt) has no metadata, `Reuploader.fetchMetadata` asks the providers in order while the media transfers (10s limit, never fails the reupload). `Reupload` returns a `Result` (links, metadata, extractor, cache hit) and stores the metadata in the manifest (`metadata`, omitted in older manifests), so cache hits keep it. The bot builds the text with `metadataText` (`bot/metadata.go`: `**title or author** first line of the description`) and only waits for and uses Discord's embed when that is empty. Spans carry `preview.metadata` and `preview.metadata_source`; the canary page shows per-site coverage.
- **`preview.ErrNoVideo`** works like `fs.SkipAll`: an extractor returns it (wrapped) only when it's sure no extractor can get anything, and `Reuploader.extract` stops trying the rest. Cobalt returns it for `error.api.fetch.empty` on X only (other cobalt services use "empty" for any fetch failure); yt-dlp returns it when it loaded an X/Bluesky/Instagram post and found no video (text or photo-only posts), but not for messages a block or login wall can also produce (e.g. Instagram's "empty media response"). Spans that end with it use `preview.EndSpan` instead of `tr.End`, which keeps an ok status and adds a `no_video` attribute/event, so error searches only show real failures (the bot's root spans set `preview.no_video`). TikTok share links (`vm.`/`vt.tiktok.com/…`, `tiktok.com/t/…`) are resolved by `resolveTikTokShortLink` (`preview/tiktok_shortlink.go`) before the cache lookup; one that points at a TikTok Shop or referral page is `ErrNoVideo`.
- **InstaFix** (`preview/extractor_instafix.go`) fetches the fixer's page for the post with a Discordbot User-Agent and without following redirects (a fixer that can't get a post redirects to instagram.com), and reads `og:video`, else `og:image`; those media urls 302 to Instagram's CDN. With carousel on, it walks `/p/<id>/1`, `/2`, … until a page has no media (max 10); reels are single pages. An extractor configured once per host names itself with a `Name()` method (`instafix:<host>`), which `extractorName` uses for traces.
- **Reddit** (`preview/extractor_reddit.go`) finds the post with `/api/info` on the OAuth API (the canary shares its `RedditAuth`), follows crossposts to the parent's video, and reads the DASH manifest for the best h264 track with a short side ≤720 plus the best audio, which `muxToMP4` merges straight from v.redd.it. `has_audio` videos never fall back to the mute `fallback_url`. Images, galleries and external links are plain errors, so the next extractor tries them.
- **URL matching** uses `simpleURLMatch` (`preview/helpers.go`): strips `https://` and `www.`, then glob-matches with `tidwall/match` patterns like `"instagram.com/reel/*"`.
- **Browser-scraping extractors** drive a third-party download site via go-rod using the helpers in `browser/browser.go`: `browser.TryChan` gives a fresh page and converts rod's `Must*` panics into errors (`panicError`: the panicked error or string itself, so `errors.Is(err, context.DeadlineExceeded)` works; only runtime errors keep a stack); `BlockThirdParty` blocks non-first-party and non-essential requests. Follow the existing extractors' shape (`page.Timeout`, `page.Race()` between success and error elements).
- **Bright Data** (`preview/extractor_brightdata.go`) maps URL patterns to hard-coded dataset IDs (Instagram reels, X posts). It calls the sync `/scrape` endpoint and polls the snapshot when collection exceeds ~60s; Instagram takes ~30-75s, X a few seconds. Its order relative to other extractors is up to the configuration.
- **yt-dlp** (`preview/extractor_ytdlp.go`) shells out to the `yt-dlp` CLI (prefers h264 ≤720p, merged to mp4), writing into the temp media dir. It runs in its own process group so timeouts also kill its ffmpeg children.
- Extractors may return `file://` URLs for locally produced files. Create them with `newTempMediaFile` (`preview/ffmpeg.go`) so the reuploader deletes them after reading. `muxToMP4` merges/remuxes one or more inputs (e.g. separate video and audio tracks) into an mp4 with ffmpeg; the Docker image includes `ffmpeg` and `yt-dlp`.
- `fetchMedia` (`preview/reuploader.go`) downloads in memory and, if the result isn't an allowed media type, falls back to remuxing the URL with `muxToMP4`. Content sniffing goes through `detectContentType`, which also accepts mp4s lacking an `mp4*` ftyp brand.
- Destinations must reject filenames containing path separators (`validateSimpleFilename`); the manifest's not-found error must satisfy `errors.Is(err, fs.ErrNotExist)` for the cache fast path to work.
- **HTTP calls** go through `httpGet`/`httpDo`/`JSONRequest[V, E]` in `preview/helpers.go`, which share a cookie jar, a browser User-Agent, a 30s timeout, and emit trace spans. Media downloads (`downloadHttp`) use a separate `mediaClient` with no total timeout instead: a download ends when no bytes arrive for 30s (`downloadIdleTimeout`) or after 5 minutes (`downloadMaxTime`), so a slow stream (e.g. through the mobile proxy) keeps going. `fetchMedia` skips its ffmpeg re-fetch after a stall, timeout or too-large file, since that would just fetch the url again; responses without a Content-Length are capped at `MaxMediaSize`. An empty body (`errEmptyMedia`) also skips the ffmpeg re-fetch.
- **Tracing**: package-level `tracer = otel.Tracer(...)`; spans are closed with `defer tr.End(span, &err)` to record the named return error. The `tr` package initializes the provider in `init()`.
- **Discord bot** (`bot/discordbot.go`): grabs the first `https://` URL in a message, waits up to 3s for Discord's own embed (used for the reply's title/description text), then replies — only using a message reference if other messages have arrived in the channel since.
- **"Video Preview" message command** (`bot/discordcommand.go`): a right-click > Apps context menu command, registered globally on ready. It defers the interaction, reuploads the target message's first link, and edits the deferred response via a raw PATCH (discordgo's `WebhookEdit` can't set the Components V2 flag). On failure the public response is deleted and replaced with an ephemeral error.
- **Canary** (`canary/`, enabled by `CANARY_SOURCE`): a goroutine started from `main.go` that keeps testing the extractor chain with real links. Every `CANARY_REFRESH` it fetches links from each `canary.Source` (currently `canary.Reddit`: the per-domain `hot` listings, e.g. `/domain/tiktok.com/hot`, via the OAuth API, skipping NSFW and removed posts and keeping only `is_video` posts for `v.redd.it`) and holds them in memory (newest 300). Tested URLs are remembered for 48h even after they're trimmed, so links still on the hot listing aren't retested. A link is kept only if `preview.ProbablyVideo` (false only for links that are definitely not video: profiles, hashtags, photo posts) and `Reuploader.IsSupported` accept it. Every `CANARY_INTERVAL` it takes the highest-scored untested link from the site tested longest ago, so rare sites aren't starved, and runs it through a second `Reuploader` that shares the real extractors but uses `preview.DiscardDestination`, so nothing is stored and the manifest cache can't skip the extractors. A check cut short by shutdown is put back to pending (`canary.outcome=canceled`) instead of failing. Each check is its own root trace `canary_check` with `canary=true`, `canary.url`, `canary.outcome` (`ok` / `no_video` / `failed`) and `site`; failures have error status, e.g. TraceQL `{name="canary_check" && status=error}`, and print a `canary: FAIL trace=... url=...` line. All links, their state and results (extractor that won, timing, Grafana trace link) are listed on the debug page from `bot/simpleserver.go`; they are in memory only. Real-traffic queries should exclude the canary (`{name="discord_reply"}` roots).
