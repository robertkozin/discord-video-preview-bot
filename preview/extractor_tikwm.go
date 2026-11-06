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

var _ Extractor = (*TikWMExtractor)(nil)

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
	Code int    `json:"code"` // 0 on success
	Msg  string `json:"msg"`
	Data *struct {
		Play   string   `json:"play"`
		HDPlay string   `json:"hdplay"`
		Images []string `json:"images"` // slideshow posts
	} `json:"data"`
}

func (ex *TikWMExtractor) Extract(ctx context.Context, mediaURL string) (_ []string, err error) {
	ctx, span := tracer.Start(ctx, "tikwm_extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	apiURL := ex.Endpoint + "?hd=1&url=" + url.QueryEscape(mediaURL)

	var parsed tikwmResponse
	for attempt := 0; attempt < 2; attempt++ {
		parsed, err = ex.fetch(ctx, apiURL)
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

	if len(parsed.Data.Images) > 0 {
		span.SetAttributes(attribute.Int("extractor.urls", len(parsed.Data.Images)))
		return parsed.Data.Images, nil
	}
	videoURL := parsed.Data.HDPlay
	if videoURL == "" {
		videoURL = parsed.Data.Play
	}
	if videoURL == "" {
		return nil, fmt.Errorf("tikwm: response has no video url")
	}
	return []string{videoURL}, nil
}

func (ex *TikWMExtractor) fetch(ctx context.Context, apiURL string) (tikwmResponse, error) {
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
