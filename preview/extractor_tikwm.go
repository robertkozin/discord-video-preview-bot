package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*TikWMExtractor)(nil)
	_ MetadataProvider = (*TikWMExtractor)(nil)
)

// TikWMExtractor uses the tikwm.com JSON API, a free third-party TikTok
// resolver. The free tier allows about one request per second, so a rate limit
// response is retried once.
type TikWMExtractor struct {
	Endpoint string
}

func NewTikWM(config *url.URL) (*TikWMExtractor, error) {
	return &TikWMExtractor{Endpoint: "https://www.tikwm.com/api/"}, nil
}

func (ex *TikWMExtractor) String() string {
	return "tikwm at " + ex.Endpoint
}

func (ex *TikWMExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"*.tiktok.com/*",
	})
}

type tikwmResponse struct {
	Code int        `json:"code"` // 0 on success
	Msg  string     `json:"msg"`
	Data *tikwmPost `json:"data"`
}

type tikwmPost struct {
	Title        string   `json:"title"` // the caption
	Play         string   `json:"play"`
	HDPlay       string   `json:"hdplay"`
	Images       []string `json:"images"` // slideshow posts
	PlayCount    int64    `json:"play_count"`
	DiggCount    int64    `json:"digg_count"`
	CommentCount int64    `json:"comment_count"`
	ShareCount   int64    `json:"share_count"`
	CreateTime   int64    `json:"create_time"`
	Author       struct {
		UniqueID string `json:"unique_id"`
		Nickname string `json:"nickname"`
	} `json:"author"`
}

func (p *tikwmPost) metadata() *Metadata {
	m := &Metadata{
		Description:  p.Title,
		Author:       p.Author.Nickname,
		AuthorHandle: p.Author.UniqueID,
		Likes:        p.DiggCount,
		Views:        p.PlayCount,
		Comments:     p.CommentCount,
		Shares:       p.ShareCount,
		Source:       "tikwm",
	}
	if p.CreateTime > 0 {
		m.PostedAt = time.Unix(p.CreateTime, 0).UTC()
	}
	return m
}

func (ex *TikWMExtractor) fetch(ctx context.Context, mediaURL string) (*tikwmPost, error) {
	span := traceSpan(ctx)
	apiURL := ex.Endpoint + "?hd=1&url=" + url.QueryEscape(mediaURL)

	var parsed tikwmResponse
	for attempt := 0; attempt < 2; attempt++ {
		var err error
		parsed, err = ex.request(ctx, apiURL)
		if err != nil {
			return nil, err
		}
		if parsed.Code == 0 || !strings.Contains(strings.ToLower(parsed.Msg), "limit") {
			break
		}
		span.AddEvent("rate_limited")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	span.SetAttributes(attribute.Int("tikwm.code", parsed.Code))

	if parsed.Code != 0 || parsed.Data == nil {
		return nil, fmt.Errorf("tikwm: %d %s", parsed.Code, parsed.Msg)
	}
	return parsed.Data, nil
}

func (ex *TikWMExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *TikWMExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "tikwm_extract")
	defer EndExtractor(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}

	if len(post.Images) > 0 {
		span.SetAttributes(attribute.Int("extractor.urls", len(post.Images)))
		return Media{URLs: post.Images, Metadata: post.metadata()}, nil
	}
	videoURL := post.HDPlay
	if videoURL == "" {
		videoURL = post.Play
	}
	if videoURL == "" {
		err = fmt.Errorf("tikwm: response has no video url")
		return Media{}, err
	}
	return Media{URLs: []string{videoURL}, Metadata: post.metadata()}, nil
}

func (ex *TikWMExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "tikwm_metadata")
	defer tr.End(span, &err)

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return post.metadata(), nil
}

func (ex *TikWMExtractor) request(ctx context.Context, apiURL string) (tikwmResponse, error) {
	var parsed tikwmResponse
	resp, err := httpGet(ctx, apiURL)
	if err != nil {
		return parsed, fmt.Errorf("fetching tikwm: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*megaByte))
	if err != nil {
		return parsed, fmt.Errorf("reading tikwm response: %s: %w", resp.Status, err)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return parsed, fmt.Errorf("parsing tikwm response: %s: %s: %w", resp.Status, truncate(string(body), 120), err)
	}
	return parsed, nil
}
