package preview

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ MediaExtractor   = (*YouTubeExtractor)(nil)
	_ MetadataProvider = (*YouTubeExtractor)(nil)
)

// YouTubeExtractor gets Shorts from YouTube's InnerTube player API as the
// visionOS app, the one client yt-dlp still uses without running YouTube's
// JavaScript or a PO token: its responses have plain media urls. Logged out,
// the API wants a visitor id (else "Sign in to confirm you're not a bot"),
// which is cached. It downloads the best h264 ≤720 and AAC audio in ranged
// chunks (a single request is throttled to about real time) and muxes them.
// Media urls only answer the IP that asked for them, so the API call and the
// downloads must leave through the same exit.
type YouTubeExtractor struct {
	// Endpoint is the site, https://www.youtube.com by default.
	Endpoint string

	mu        sync.Mutex
	visitor   string
	visitorAt time.Time
}

const (
	youtubeClientName    = "VISIONOS"
	youtubeClientID      = "101" // X-YouTube-Client-Name for VISIONOS
	youtubeClientVersion = "1.02"
	youtubeUserAgent     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15"
	// youtubeVisitorTTL is how long a visitor id is reused.
	youtubeVisitorTTL = 6 * time.Hour
	// youtubeChunk is the ranged download size; yt-dlp uses the same.
	youtubeChunk = 10 << 20
)

func NewYouTube(config *url.URL) (*YouTubeExtractor, error) {
	endpoint := "https://www.youtube.com"
	// youtube://host:port?insecure=1 points elsewhere, e.g. a test server
	if config.Host != "" {
		scheme := "https"
		if config.Query().Has("insecure") {
			scheme = "http"
		}
		endpoint = scheme + "://" + config.Host
	}
	return &YouTubeExtractor{Endpoint: endpoint}, nil
}

func (ex *YouTubeExtractor) String() string {
	return "youtube at " + ex.Endpoint
}

func (ex *YouTubeExtractor) IsSupported(mediaURL string) bool {
	_, ok := youtubeShortID(mediaURL)
	return ok
}

var youtubeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youtubeShortID returns the video id of a youtube.com/shorts/<id> url.
func youtubeShortID(mediaURL string) (string, bool) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "youtube.com" && host != "m.youtube.com" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "shorts" || !youtubeIDPattern.MatchString(parts[1]) {
		return "", false
	}
	return parts[1], true
}

type youtubePlayer struct {
	PlayabilityStatus struct {
		Status string `json:"status"` // OK, LOGIN_REQUIRED, UNPLAYABLE, ERROR…
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	VideoDetails struct {
		Title            string `json:"title"`
		Author           string `json:"author"`
		ShortDescription string `json:"shortDescription"`
		ViewCount        string `json:"viewCount"`
	} `json:"videoDetails"`
	StreamingData struct {
		Formats         []youtubeFormat `json:"formats"` // muxed (video+audio)
		AdaptiveFormats []youtubeFormat `json:"adaptiveFormats"`
	} `json:"streamingData"`
}

type youtubeFormat struct {
	Itag          int    `json:"itag"`
	URL           string `json:"url"` // empty when the url is ciphered
	MimeType      string `json:"mimeType"`
	Bitrate       int64  `json:"bitrate"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ContentLength string `json:"contentLength"`
	IsDrc         bool   `json:"isDrc"` // dynamic range compressed copy of an audio track
}

func (p *youtubePlayer) metadata() *Metadata {
	views, _ := strconv.ParseInt(p.VideoDetails.ViewCount, 10, 64)
	return &Metadata{
		Title:       p.VideoDetails.Title,
		Description: p.VideoDetails.ShortDescription,
		Author:      p.VideoDetails.Author,
		Views:       views,
		Source:      "youtube",
	}
}

// visitorID returns a cached visitor id, or fetches a new one when it's old
// or refresh is set.
func (ex *YouTubeExtractor) visitorID(ctx context.Context, refresh bool) (string, error) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if !refresh && ex.visitor != "" && time.Since(ex.visitorAt) < youtubeVisitorTTL {
		return ex.visitor, nil
	}
	v, err := ex.fetchVisitorID(ctx)
	if err != nil {
		return "", err
	}
	ex.visitor, ex.visitorAt = v, time.Now()
	traceSpan(ctx).AddEvent("visitor_id_fetched")
	return v, nil
}

var (
	// a visitor id is a base64 protobuf, url-escaped, starting with "Cg"
	youtubeVisitorPattern  = regexp.MustCompile(`"(Cg[A-Za-z0-9_%-]{10,})"`)
	youtubeVisitorDataPage = regexp.MustCompile(`"VISITOR_DATA":"([^"]+)"`)
)

// fetchVisitorID reads one from /sw.js_data (2.7KB, what the web player's
// service worker uses), else from a watch page's ytcfg (1.3MB).
func (ex *YouTubeExtractor) fetchVisitorID(ctx context.Context) (string, error) {
	body, err := ex.get(ctx, ex.Endpoint+"/sw.js_data", megaByte)
	if err == nil {
		if m := youtubeVisitorPattern.FindSubmatch(body); m != nil {
			if v, err := url.QueryUnescape(string(m[1])); err == nil {
				return v, nil
			}
		}
	}
	body, err2 := ex.get(ctx, ex.Endpoint+"/watch?v=jNQXAC9IVRw", 8*megaByte)
	if err2 == nil {
		if m := youtubeVisitorDataPage.FindSubmatch(body); m != nil {
			return string(m[1]), nil
		}
		err2 = errors.New("no VISITOR_DATA on the watch page")
	}
	return "", fmt.Errorf("youtube: getting a visitor id: %w", errors.Join(err, err2))
}

func (ex *YouTubeExtractor) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := httpDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, truncate(string(body), 120))
	}
	return body, nil
}

// player asks the player API about a video, getting a new visitor id once if
// YouTube asks for a bot check.
func (ex *YouTubeExtractor) player(ctx context.Context, id string) (*youtubePlayer, error) {
	span := traceSpan(ctx)
	var p *youtubePlayer
	for attempt := 0; attempt < 2; attempt++ {
		visitor, err := ex.visitorID(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		body := map[string]any{
			"context": map[string]any{"client": map[string]any{
				"clientName": youtubeClientName, "clientVersion": youtubeClientVersion,
				"deviceMake": "Apple", "deviceModel": "RealityDevice17,1", "userAgent": youtubeUserAgent,
				"osName": "visionOS", "osVersion": "26.5.23O471", "hl": "en", "gl": "US", "visitorData": visitor,
			}},
			"videoId": id, "contentCheckOk": true, "racyCheckOk": true,
		}
		_, p, err = JSONRequest[youtubePlayer, youtubeAPIError](ctx, http.MethodPost, ex.Endpoint+"/youtubei/v1/player?prettyPrint=false", body,
			"User-Agent", youtubeUserAgent,
			"X-YouTube-Client-Name", youtubeClientID,
			"X-YouTube-Client-Version", youtubeClientVersion,
			"X-Goog-Visitor-Id", visitor,
			"Origin", ex.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("youtube player: %w", err)
		}
		if p.PlayabilityStatus.Status == "LOGIN_REQUIRED" && strings.Contains(p.PlayabilityStatus.Reason, "bot") && attempt == 0 {
			span.AddEvent("bot_check", trace.WithAttributes(attribute.String("reason", p.PlayabilityStatus.Reason)))
			continue
		}
		break
	}
	span.SetAttributes(attribute.String("youtube.status", p.PlayabilityStatus.Status))

	switch p.PlayabilityStatus.Status {
	case "OK":
		return p, nil
	case "ERROR":
		// "This video is unavailable" / removed: nobody can get it
		return nil, fmt.Errorf("youtube: %s: %w", p.PlayabilityStatus.Reason, ErrNoVideo)
	default:
		// LOGIN_REQUIRED (age or bot check), UNPLAYABLE (region, made for kids on
		// this client)… another extractor may still manage
		return nil, fmt.Errorf("youtube: %s: %s", p.PlayabilityStatus.Status, p.PlayabilityStatus.Reason)
	}
}

type youtubeAPIError struct {
	Body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e youtubeAPIError) Error() string { return fmt.Sprintf("%d %s", e.Body.Code, e.Body.Message) }

// pickYouTubeFormats picks the best h264 whose short side is ≤720 (or the
// smallest h264 when they're all bigger) and the best AAC, skipping
// dynamic-range-compressed copies. VP9 and AV1 don't play everywhere in
// Discord. Without both, it falls back to a muxed h264 format.
func pickYouTubeFormats(p *youtubePlayer) (video, audio *youtubeFormat) {
	var h264, aac []youtubeFormat
	for _, f := range p.StreamingData.AdaptiveFormats {
		switch {
		case f.URL == "":
		case strings.HasPrefix(f.MimeType, `video/mp4; codecs="avc1`):
			h264 = append(h264, f)
		case strings.HasPrefix(f.MimeType, "audio/mp4") && !f.IsDrc:
			aac = append(aac, f)
		}
	}
	byBitrate := func(a, b youtubeFormat) int { return cmp.Compare(b.Bitrate, a.Bitrate) }
	slices.SortFunc(h264, byBitrate)
	slices.SortFunc(aac, byBitrate)
	if len(h264) > 0 && len(aac) > 0 {
		video = &h264[len(h264)-1]
		for i, f := range h264 {
			if min(f.Width, f.Height) <= 720 {
				video = &h264[i]
				break
			}
		}
		return video, &aac[0]
	}
	for i, f := range p.StreamingData.Formats {
		if f.URL != "" && strings.HasPrefix(f.MimeType, `video/mp4; codecs="avc1`) {
			return &p.StreamingData.Formats[i], nil
		}
	}
	return nil, nil
}

// download fetches a format in youtubeChunk ranges into a temp file and
// returns its path.
func (ex *YouTubeExtractor) download(ctx context.Context, f *youtubeFormat, ext string) (_ string, err error) {
	size, _ := strconv.ParseInt(f.ContentLength, 10, 64)
	if size <= 0 {
		return "", fmt.Errorf("youtube: format %d has no content length", f.Itag)
	}
	if size > MaxMediaSize {
		return "", fmt.Errorf("%w: %d bytes", errMediaTooLarge, size)
	}
	path, err := newTempMediaFile("youtube-*" + ext)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			os.Remove(path)
		}
	}()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	defer file.Close()

	header := http.Header{"User-Agent": {youtubeUserAgent}}
	for start := int64(0); start < size; start += youtubeChunk {
		end := min(start+youtubeChunk, size) - 1
		chunk, err := downloadHttpWith(ctx, mediaClient, f.URL+"&range="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10), header)
		if err != nil {
			return "", fmt.Errorf("downloading youtube format %d: %w", f.Itag, err)
		}
		if int64(len(chunk)) != end-start+1 {
			return "", fmt.Errorf("downloading youtube format %d: got %d bytes for range %d-%d", f.Itag, len(chunk), start, end)
		}
		if _, err := file.Write(chunk); err != nil {
			return "", err
		}
	}
	return path, file.Close()
}

func (ex *YouTubeExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *YouTubeExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "youtube_extract")
	defer EndExtractor(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	id, ok := youtubeShortID(mediaURL)
	if !ok {
		return Media{}, fmt.Errorf("not a youtube short url: %s", mediaURL)
	}
	p, err := ex.player(ctx, id)
	if err != nil {
		return Media{}, err
	}

	video, audio := pickYouTubeFormats(p)
	if video == nil {
		return Media{}, errors.New("youtube: no h264 format with a plain url")
	}
	span.SetAttributes(attribute.Int("youtube.video_itag", video.Itag))

	videoPath, err := ex.download(ctx, video, ".mp4")
	if err != nil {
		return Media{}, err
	}
	if audio == nil {
		// a muxed format: already has its audio
		return Media{URLs: []string{"file://" + videoPath}, Metadata: p.metadata()}, nil
	}
	defer os.Remove(videoPath)
	span.SetAttributes(attribute.Int("youtube.audio_itag", audio.Itag))

	audioPath, err := ex.download(ctx, audio, ".m4a")
	if err != nil {
		return Media{}, err
	}
	defer os.Remove(audioPath)

	muxed, err := muxToMP4(ctx, videoPath, audioPath)
	if err != nil {
		return Media{}, err
	}
	return Media{URLs: []string{muxed}, Metadata: p.metadata()}, nil
}

func (ex *YouTubeExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "youtube_metadata")
	defer tr.End(span, &err)

	id, ok := youtubeShortID(mediaURL)
	if !ok {
		return nil, fmt.Errorf("not a youtube short url: %s", mediaURL)
	}
	p, err := ex.player(ctx, id)
	if err != nil {
		return nil, err
	}
	return p.metadata(), nil
}
