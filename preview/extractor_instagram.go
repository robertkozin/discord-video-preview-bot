package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*InstagramExtractor)(nil)
	_ MetadataProvider = (*InstagramExtractor)(nil)
)

// InstagramExtractor reads posts from Instagram's mobile API
// (i.instagram.com/api/v1/media/<id>/info/) with a logged-in session cookie.
// Logged out, Instagram serves only an empty JS shell for posts, answers its
// GraphQL endpoint with 401 "require_login" and has no media in embeds, so a
// (throwaway) account is needed. The media id is decoded from the shortcode
// (no extra request), and the CDN urls in the response are signed, so they
// download like any other url.
//
//	instagram://?cookies=/cookies.json   cobalt's file: {"instagram": ["sessionid=…; csrftoken=…"]}, read on every request
//	instagram://?cookie=sessionid%3D…    the Cookie header itself
//	instagram://localhost:8080?insecure=1&cookie=… (a test server)
//
// A dead session (login_required, checkpoint) is a plain error, so the next
// extractor still tries; a post that is gone is ErrNoVideo.
type InstagramExtractor struct {
	Endpoint   string // https://i.instagram.com by default
	Cookie     string
	CookieFile string
}

const instagramUserAgent = "Instagram 275.0.0.27.98 Android (33/13; 280dpi; 720x1423; Xiaomi; Redmi 7; onclite; qcom; en_US; 458229237)"

// instagramMaxItems caps a carousel; a Discord media gallery holds 10.
const instagramMaxItems = 10

func NewInstagram(config *url.URL) (*InstagramExtractor, error) {
	q := config.Query()
	ex := &InstagramExtractor{Endpoint: "https://i.instagram.com", Cookie: q.Get("cookie"), CookieFile: q.Get("cookies")}
	if config.Host != "" {
		scheme := "https"
		if q.Has("insecure") {
			scheme = "http"
		}
		ex.Endpoint = scheme + "://" + config.Host
	}
	if ex.Cookie == "" && ex.CookieFile == "" {
		return nil, fmt.Errorf("instagram needs a session: instagram://?cookies=/cookies.json or ?cookie=…")
	}
	if _, err := ex.cookie(); err != nil {
		return nil, err
	}
	return ex, nil
}

func (ex *InstagramExtractor) String() string { return "instagram at " + ex.Endpoint }

// cookie returns the Cookie header, rereading the file since cobalt rewrites
// refreshed tokens into it.
func (ex *InstagramExtractor) cookie() (string, error) {
	if ex.CookieFile == "" {
		return ex.Cookie, nil
	}
	b, err := os.ReadFile(ex.CookieFile)
	if err != nil {
		return "", fmt.Errorf("reading instagram cookies: %w", err)
	}
	var file map[string][]string
	if err := json.Unmarshal(b, &file); err != nil {
		return "", fmt.Errorf("parsing instagram cookies: %w", err)
	}
	if len(file["instagram"]) == 0 || file["instagram"][0] == "" {
		return "", fmt.Errorf("no instagram cookie in %s", ex.CookieFile)
	}
	return file["instagram"][0], nil
}

func (ex *InstagramExtractor) IsSupported(mediaURL string) bool {
	_, err := instagramPostCode(mediaURL)
	return err == nil
}

// instagramPostCode returns the shortcode of a post, reel or tv url, also
// under a username (instagram.com/<user>/reel/<code>).
func instagramPostCode(mediaURL string) (string, error) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", err
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "instagram.com" {
		return "", fmt.Errorf("not an instagram url: %s", mediaURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		if (p == "p" || p == "reel" || p == "reels" || p == "tv") && i+1 < len(parts) && i <= 1 {
			if code := parts[i+1]; len(code) >= 11 && instagramCodeChars(code) {
				return code, nil
			}
		}
	}
	return "", fmt.Errorf("not an instagram post url: %s", mediaURL)
}

func instagramCodeChars(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

const instagramAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// instagramMediaID decodes a shortcode into the numeric media id: the
// shortcode is the id in base 64. Newer posts' shortcodes have a suffix, so
// only the first 11 characters count.
func instagramMediaID(code string) (string, error) {
	code = code[:min(len(code), 11)]
	var id uint64
	for _, c := range code {
		i := strings.IndexRune(instagramAlphabet, c)
		if i < 0 {
			return "", fmt.Errorf("bad instagram shortcode %q", code)
		}
		id = id<<6 | uint64(i)
	}
	return strconv.FormatUint(id, 10), nil
}

type instagramItem struct {
	Code         string `json:"code"`
	MediaType    int    `json:"media_type"` // 1 photo, 2 video, 8 carousel
	TakenAt      int64  `json:"taken_at"`
	LikeCount    int64  `json:"like_count"`
	CommentCount int64  `json:"comment_count"`
	PlayCount    int64  `json:"play_count"`
	ViewCount    int64  `json:"view_count"`
	Caption      *struct {
		Text string `json:"text"`
	} `json:"caption"`
	User struct {
		Username string `json:"username"`
		FullName string `json:"full_name"`
	} `json:"user"`
	VideoVersions []instagramMedia `json:"video_versions"`
	ImageVersions struct {
		Candidates []instagramMedia `json:"candidates"`
	} `json:"image_versions2"`
	CarouselMedia []instagramItem `json:"carousel_media"`
}

type instagramMedia struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type instagramResponse struct {
	Items   []instagramItem `json:"items"`
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Login   bool            `json:"require_login"`
}

func (ex *InstagramExtractor) fetch(ctx context.Context, mediaURL string) (*instagramItem, error) {
	code, err := instagramPostCode(mediaURL)
	if err != nil {
		return nil, err
	}
	id, err := instagramMediaID(code)
	if err != nil {
		return nil, err
	}
	cookie, err := ex.cookie()
	if err != nil {
		return nil, err
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ex.Endpoint+"/api/v1/media/"+id+"/info/", nil)
	req.Header.Set("User-Agent", instagramUserAgent)
	req.Header.Set("X-IG-App-ID", "936619743392459")
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("Cookie", cookie)
	resp, err := httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("fetching instagram media info: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading instagram media info: %s: %w", resp.Status, err)
	}

	var info instagramResponse
	_ = json.Unmarshal(body, &info)
	if resp.StatusCode == http.StatusOK && len(info.Items) > 0 {
		return &info.Items[0], nil
	}
	msg := cmp.Or(info.Message, truncate(string(body), 120))
	switch {
	case info.Login || resp.StatusCode == http.StatusUnauthorized || strings.Contains(msg, "login_required") || strings.Contains(msg, "checkpoint"):
		return nil, fmt.Errorf("instagram session rejected (login required): %s: %s", resp.Status, msg)
	case resp.StatusCode == http.StatusNotFound || strings.Contains(strings.ToLower(msg), "media not found"):
		return nil, fmt.Errorf("instagram media info: %s: %w", msg, ErrNoVideo)
	case resp.StatusCode == http.StatusOK:
		// 200 without items: deleted, or not visible to this account
		return nil, fmt.Errorf("instagram media info: no items: %s", msg)
	}
	return nil, fmt.Errorf("instagram media info: %s: %s", resp.Status, msg)
}

// pickInstagramVideo picks the first video version whose short side is ≤720
// (the others are bigger renditions), or the smallest when all are bigger.
func pickInstagramVideo(versions []instagramMedia) string {
	var best, smallest *instagramMedia
	for i := range versions {
		v := &versions[i]
		if v.URL == "" {
			continue
		}
		if smallest == nil || min(v.Width, v.Height) < min(smallest.Width, smallest.Height) {
			smallest = v
		}
		if min(v.Width, v.Height) <= 720 && (best == nil || min(v.Width, v.Height) > min(best.Width, best.Height)) {
			best = v
		}
	}
	if best == nil {
		best = smallest
	}
	if best == nil {
		return ""
	}
	return best.URL
}

// urls returns one url per video or photo, and whether any was a video.
func (it *instagramItem) urls() (urls []string, video bool) {
	items := it.CarouselMedia
	if len(items) == 0 {
		items = []instagramItem{*it}
	}
	for _, m := range items {
		if len(urls) == instagramMaxItems {
			break
		}
		if v := pickInstagramVideo(m.VideoVersions); v != "" {
			urls = append(urls, v)
			video = true
		} else if c := m.ImageVersions.Candidates; len(c) > 0 && c[0].URL != "" {
			urls = append(urls, c[0].URL) // largest first
		}
	}
	return urls, video
}

func (it *instagramItem) metadata() *Metadata {
	m := &Metadata{
		Author:       it.User.FullName,
		AuthorHandle: it.User.Username,
		Likes:        it.LikeCount,
		Views:        cmp.Or(it.PlayCount, it.ViewCount),
		Comments:     it.CommentCount,
		Source:       "instagram",
	}
	if it.Caption != nil {
		m.Description = it.Caption.Text
	}
	if it.TakenAt > 0 {
		m.PostedAt = time.Unix(it.TakenAt, 0).UTC()
	}
	return m
}

func (ex *InstagramExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *InstagramExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "instagram_extract")
	defer EndSpan(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	item, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}
	urls, _ := item.urls()
	if len(urls) == 0 {
		err = errors.New("instagram media info: no media in post")
		return Media{}, err
	}
	span.SetAttributes(attribute.Int("extractor.urls", len(urls)), attribute.Int("instagram.media_type", item.MediaType))
	return Media{URLs: urls, Metadata: item.metadata()}, nil
}

func (ex *InstagramExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "instagram_metadata")
	defer tr.End(span, &err)

	item, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return item.metadata(), nil
}
