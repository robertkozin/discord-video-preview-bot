package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var _ Extractor = (*CobaltExtractor)(nil)

type CobaltExtractor struct {
	Endpoint string
	APIKey   string
}

func NewCobalt(config *url.URL) (*CobaltExtractor, error) {
	endpoint := *config
	query := config.Query()

	endpoint.RawQuery = ""

	endpoint.Scheme = "https"
	if query.Has("insecure") {
		endpoint.Scheme = "http"
	}

	apiKey := query.Get("key")

	return &CobaltExtractor{
		Endpoint: endpoint.String(),
		APIKey:   apiKey,
	}, nil
}

func (c *CobaltExtractor) String() string {
	return fmt.Sprintf("cobalt.tools at %s", c.Endpoint)
}

func (c *CobaltExtractor) IsSupported(url string) bool {
	return simpleURLMatch(url, []string{
		"instagram.com/reel/*",
		"instagram.com/reels/*",
		"instagram.com/p/*",
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"*.tiktok.com/*",
		"twitter.com/*/status/*",
		"t.co/*",
		"x.com/*/status/*",
		"bsky.app/profile/*/post/*",
		"twitch.tv/*/clip/*",
		"youtube.com/shorts/*",
		"reddit.com/r/*/comments/*",
		"old.reddit.com/r/*/comments/*",
		"redd.it/*",
		"v.redd.it/*",
	})
}

type CobaltRequest struct {
	Url string `json:"url"`
}

type CobaltError struct {
	Status string `json:"status"`
	Err    struct {
		Code    string          `json:"code"`
		Context json.RawMessage `json:"context"`
	} `json:"error"`
}

func (ce CobaltError) Error() string {
	if len(ce.Err.Context) > 0 && string(ce.Err.Context) != "null" {
		return fmt.Sprintf("cobalt error: %s %s", ce.Err.Code, ce.Err.Context)
	}
	return "cobalt error: " + ce.Err.Code
}

type CobaltResponse struct {
	Status string         `json:"status"` // tunnel / local-processing / redirect / picker / error
	Url    string         `json:"url"`
	Picker []CobaltPicker `json:"picker"`
	Audio  any            `json:"audio"`
}

type CobaltPicker struct {
	Type string `json:"type"` // photo / video / gif
	Url  string `json:"url"`
}

func (ex *CobaltExtractor) Extract(ctx context.Context, url string) (_ []string, err error) {
	var (
		req     = CobaltRequest{Url: url}
		headers []string
	)
	ctx, span := tracer.Start(ctx, "cobalt_extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(url)))

	if ex.APIKey != "" {
		headers = []string{"Authorization", "Api-Key " + ex.APIKey}
	}

	_, value, err := JSONRequest[CobaltResponse, CobaltError](ctx, "POST", ex.Endpoint, req, headers...)
	if err != nil {
		// for x, cobalt only says "empty" once both its graphql and syndication
		// lookups found no media (private/unavailable posts have their own codes).
		// other services use "empty" for any fetch failure, so don't trust it there.
		var cobaltErr CobaltError
		if errors.As(err, &cobaltErr) {
			span.SetAttributes(attribute.String("cobalt.error_code", cobaltErr.Err.Code))
		}
		if errors.As(err, &cobaltErr) && cobaltErr.Err.Code == "error.api.fetch.empty" &&
			simpleURLMatch(url, []string{"x.com/*/status/*", "twitter.com/*/status/*"}) {
			return nil, fmt.Errorf("cobalt: %w", ErrNoVideo)
		}
		return nil, fmt.Errorf("making cobalt request: %w", err)
	}

	span.SetAttributes(attribute.String("cobalt.status", value.Status))
	switch value.Status {
	case "redirect":
		return []string{value.Url}, nil
	case "tunnel":
		return []string{value.Url}, nil
	case "picker":
		urls := make([]string, len(value.Picker))
		for i, p := range value.Picker {
			urls[i] = p.Url
		}
		return urls, nil
	default:
		return nil, fmt.Errorf("unexpected cobalt response type: %s", value.Status)
	}
}
