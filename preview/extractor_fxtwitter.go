package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var _ Extractor = (*FxTwitterExtractor)(nil)

// FxTwitterExtractor reads X/Twitter media urls from the FxEmbed (FixTweet)
// JSON API. It needs no key and is plain HTTP, so it is quick and cheap.
type FxTwitterExtractor struct {
	// Endpoint is the API base url, https://api.fxtwitter.com by default.
	Endpoint string
}

func NewFxTwitter(config *url.URL) (*FxTwitterExtractor, error) {
	endpoint := "https://api.fxtwitter.com"
	// fxtwitter://host:port?insecure=1 points at a self-hosted FxEmbed
	if config.Host != "" {
		scheme := "https"
		if config.Query().Has("insecure") {
			scheme = "http"
		}
		endpoint = scheme + "://" + config.Host
	}
	return &FxTwitterExtractor{Endpoint: endpoint}, nil
}

func (ex *FxTwitterExtractor) String() string {
	return "fxtwitter at " + ex.Endpoint
}

func (ex *FxTwitterExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"x.com/*/status/*",
		"twitter.com/*/status/*",
	})
}

type fxTweetResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Tweet   *struct {
		Media *struct {
			All []struct {
				Type string `json:"type"` // photo / video / gif
				URL  string `json:"url"`
			} `json:"all"`
		} `json:"media"`
	} `json:"tweet"`
}

// tweetPath returns "<user>/status/<id>" for an x.com or twitter.com status url.
func tweetPath(mediaURL string) (string, error) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[1] != "status" || parts[0] == "" || parts[2] == "" {
		return "", fmt.Errorf("not a tweet url: %s", mediaURL)
	}
	return parts[0] + "/status/" + parts[2], nil
}

func (ex *FxTwitterExtractor) Extract(ctx context.Context, mediaURL string) (_ []string, err error) {
	ctx, span := tracer.Start(ctx, "fxtwitter_extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	path, err := tweetPath(mediaURL)
	if err != nil {
		return nil, err
	}

	resp, err := httpGet(ctx, ex.Endpoint+"/"+path)
	if err != nil {
		return nil, fmt.Errorf("fetching fxtwitter: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading fxtwitter response: %s: %w", resp.Status, err)
	}

	var parsed fxTweetResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// e.g. an HTML block page instead of JSON
		return nil, fmt.Errorf("parsing fxtwitter response: %s: %s: %w", resp.Status, truncate(string(body), 120), err)
	}
	span.SetAttributes(attribute.Int("fxtwitter.code", parsed.Code))

	if parsed.Code != 200 || parsed.Tweet == nil {
		return nil, fmt.Errorf("fxtwitter: %d %s", parsed.Code, parsed.Message)
	}

	var urls []string
	if parsed.Tweet.Media != nil {
		for _, m := range parsed.Tweet.Media.All {
			if m.URL != "" {
				urls = append(urls, m.URL)
			}
		}
	}
	if len(urls) == 0 {
		// FxTwitter loaded the tweet and it has no media (text-only post)
		return nil, fmt.Errorf("fxtwitter: %w", ErrNoVideo)
	}

	span.SetAttributes(attribute.Int("extractor.urls", len(urls)))
	return urls, nil
}
