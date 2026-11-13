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
	_ MediaExtractor   = (*FxTwitterExtractor)(nil)
	_ MetadataProvider = (*FxTwitterExtractor)(nil)
)

// FxTwitterExtractor reads X/Twitter media urls and post details from the
// FxEmbed (FixTweet) JSON API. It needs no key and is plain HTTP, so it is
// quick and cheap.
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
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Tweet   *fxTweet `json:"tweet"`
}

type fxTweet struct {
	Text             string `json:"text"`
	Likes            int64  `json:"likes"`
	Retweets         int64  `json:"retweets"`
	Replies          int64  `json:"replies"`
	Views            int64  `json:"views"`
	CreatedTimestamp int64  `json:"created_timestamp"`
	Author           struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"author"`
	Media *struct {
		All []struct {
			Type string `json:"type"` // photo / video / gif
			URL  string `json:"url"`
		} `json:"all"`
	} `json:"media"`
}

func (t *fxTweet) metadata() *Metadata {
	m := &Metadata{
		Description:  t.Text,
		Author:       t.Author.Name,
		AuthorHandle: t.Author.ScreenName,
		Likes:        t.Likes,
		Views:        t.Views,
		Comments:     t.Replies,
		Shares:       t.Retweets,
		Source:       "fxtwitter",
	}
	if t.CreatedTimestamp > 0 {
		m.PostedAt = time.Unix(t.CreatedTimestamp, 0).UTC()
	}
	return m
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

func (ex *FxTwitterExtractor) fetch(ctx context.Context, mediaURL string) (*fxTweet, error) {
	span := traceSpan(ctx)
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
	return parsed.Tweet, nil
}

func (ex *FxTwitterExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *FxTwitterExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "fxtwitter_extract")
	defer EndExtractor(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	tweet, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}

	var urls []string
	if tweet.Media != nil {
		for _, m := range tweet.Media.All {
			if m.URL != "" {
				urls = append(urls, m.URL)
			}
		}
	}
	if len(urls) == 0 {
		// FxTwitter loaded the tweet and it has no media (text-only post)
		err = fmt.Errorf("fxtwitter: %w", ErrNoVideo)
		return Media{}, err
	}

	span.SetAttributes(attribute.Int("extractor.urls", len(urls)))
	return Media{URLs: urls, Metadata: tweet.metadata()}, nil
}

func (ex *FxTwitterExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "fxtwitter_metadata")
	defer tr.End(span, &err)

	tweet, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return tweet.metadata(), nil
}
