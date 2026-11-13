package preview

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*XExtractor)(nil)
	_ MetadataProvider = (*XExtractor)(nil)
)

// XExtractor reads X/Twitter posts straight from X's embed (syndication) API,
// the JSON behind embedded tweets on other sites. It needs no account or
// key. It can't see posts that need a login (age-restricted and some
// sensitive posts come back as a TweetTombstone), and long posts' text is
// cut at ~280 characters, so keep fxtwitter after it as a fallback.
type XExtractor struct {
	// Endpoint is the API base url, https://cdn.syndication.twimg.com by default.
	Endpoint string
}

func NewX(config *url.URL) (*XExtractor, error) {
	endpoint := "https://cdn.syndication.twimg.com"
	// x://host:port?insecure=1 points elsewhere, e.g. a test server
	if config.Host != "" {
		scheme := "https"
		if config.Query().Has("insecure") {
			scheme = "http"
		}
		endpoint = scheme + "://" + config.Host
	}
	return &XExtractor{Endpoint: endpoint}, nil
}

func (ex *XExtractor) String() string {
	return "x at " + ex.Endpoint
}

func (ex *XExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"x.com/*/status/*",
		"twitter.com/*/status/*",
	})
}

type xTweet struct {
	Typename         string    `json:"__typename"` // Tweet, or TweetTombstone when it needs a login
	Text             string    `json:"text"`
	DisplayTextRange [2]int    `json:"display_text_range"` // UTF-16 offsets into Text
	CreatedAt        string    `json:"created_at"`
	FavoriteCount    int64     `json:"favorite_count"`
	ConversationCnt  int64     `json:"conversation_count"`
	NoteTweet        *struct{} `json:"note_tweet"` // set when Text is cut short
	Entities         struct {
		URLs []struct {
			URL         string `json:"url"`
			ExpandedURL string `json:"expanded_url"`
		} `json:"urls"`
		Media []struct {
			URL string `json:"url"`
		} `json:"media"`
	} `json:"entities"`
	User struct {
		Name       string `json:"name"`
		ScreenName string `json:"screen_name"`
	} `json:"user"`
	MediaDetails []xMedia       `json:"mediaDetails"`
	QuotedTweet  *xTweet        `json:"quoted_tweet"`
	Card         map[string]any `json:"card"`
}

type xMedia struct {
	Type          string `json:"type"` // photo / video / animated_gif
	MediaURLHTTPS string `json:"media_url_https"`
	VideoInfo     struct {
		Variants []xVariant `json:"variants"`
	} `json:"video_info"`
}

type xVariant struct {
	ContentType string `json:"content_type"`
	Bitrate     int64  `json:"bitrate"`
	URL         string `json:"url"`
}

// tweetID returns the numeric id of an x.com or twitter.com status url.
func tweetID(mediaURL string) (string, error) {
	path, err := tweetPath(mediaURL)
	if err != nil {
		return "", err
	}
	id := path[strings.LastIndex(path, "/")+1:]
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", fmt.Errorf("not a tweet url: %s", mediaURL)
	}
	return id, nil
}

func (ex *XExtractor) fetch(ctx context.Context, mediaURL string) (*xTweet, error) {
	span := traceSpan(ctx)
	id, err := tweetID(mediaURL)
	if err != nil {
		return nil, err
	}

	// token must be non-empty (an empty one gets {}), but its value isn't checked
	resp, err := httpGet(ctx, ex.Endpoint+"/tweet-result?id="+id+"&lang=en&token=x")
	if err != nil {
		return nil, fmt.Errorf("fetching x embed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading x embed response: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 404 for deleted or private posts
		return nil, fmt.Errorf("x embed: %s: %s", resp.Status, truncate(string(body), 120))
	}

	var tweet xTweet
	if err := json.Unmarshal(body, &tweet); err != nil {
		return nil, fmt.Errorf("parsing x embed response: %s: %w", truncate(string(body), 120), err)
	}
	span.SetAttributes(attribute.String("x.typename", tweet.Typename))
	if tweet.Typename != "Tweet" {
		// TweetTombstone: X only shows it logged in; fxtwitter usually still can
		return nil, fmt.Errorf("x embed: post unavailable without login (%q)", tweet.Typename)
	}
	return &tweet, nil
}

// mediaURLs returns one url per photo, video or gif of the post.
func (t *xTweet) mediaURLs() []string {
	var urls []string
	for _, m := range t.MediaDetails {
		switch m.Type {
		case "photo":
			if m.MediaURLHTTPS != "" {
				urls = append(urls, m.MediaURLHTTPS+"?name=orig")
			}
		case "video", "animated_gif":
			if v := pickXVariant(m.VideoInfo.Variants); v != "" {
				urls = append(urls, v)
			}
		}
	}
	return urls
}

var xVariantSize = regexp.MustCompile(`/(\d+)x(\d+)/`)

// pickXVariant picks the best mp4 whose short side is ≤720 (as the other
// extractors do), or the smallest mp4 when they are all bigger. The size comes
// from the url (".../vid/avc1/720x1280/..."); variants without one count as
// small enough.
func pickXVariant(variants []xVariant) string {
	var mp4s []xVariant
	for _, v := range variants {
		if v.ContentType == "video/mp4" && v.URL != "" {
			mp4s = append(mp4s, v)
		}
	}
	if len(mp4s) == 0 {
		return ""
	}
	slices.SortFunc(mp4s, func(a, b xVariant) int { return cmp.Compare(b.Bitrate, a.Bitrate) })
	for _, v := range mp4s {
		if m := xVariantSize.FindStringSubmatch(v.URL); m != nil {
			w, _ := strconv.Atoi(m[1])
			h, _ := strconv.Atoi(m[2])
			if min(w, h) > 720 {
				continue
			}
		}
		return v.URL
	}
	return mp4s[len(mp4s)-1].URL
}

// text is the post's text as shown: without the trailing media links, with
// t.co links expanded and HTML entities decoded.
func (t *xTweet) text() string {
	text := t.Text
	units := utf16.Encode([]rune(text))
	if start, end := t.DisplayTextRange[0], t.DisplayTextRange[1]; 0 <= start && start < end && end <= len(units) {
		text = string(utf16.Decode(units[start:end]))
	}
	for _, u := range t.Entities.URLs {
		if u.URL != "" && u.ExpandedURL != "" {
			text = strings.ReplaceAll(text, u.URL, u.ExpandedURL)
		}
	}
	// media borrowed from another post is inside the display range
	for _, m := range t.Entities.Media {
		if m.URL != "" {
			text = strings.ReplaceAll(text, m.URL, "")
		}
	}
	text = strings.TrimSpace(html.UnescapeString(text))
	if t.NoteTweet != nil {
		text += "…"
	}
	return text
}

func (t *xTweet) metadata() *Metadata {
	m := &Metadata{
		Description:  t.text(),
		Author:       t.User.Name,
		AuthorHandle: t.User.ScreenName,
		Likes:        t.FavoriteCount,
		Comments:     t.ConversationCnt,
		Source:       "x",
	}
	if posted, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil {
		m.PostedAt = posted.UTC()
	}
	return m
}

func (ex *XExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *XExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "x_extract")
	defer EndSpan(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	tweet, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}

	urls := tweet.mediaURLs()
	if len(urls) == 0 && tweet.QuotedTweet != nil {
		// a post quoting a video: show the quoted video
		urls = tweet.QuotedTweet.mediaURLs()
		span.SetAttributes(attribute.Bool("x.quoted_media", len(urls) > 0))
	}
	if len(urls) == 0 {
		if tweet.Card != nil {
			// cards (link previews, some promoted videos) aren't read here, let others try
			err = fmt.Errorf("x embed: post has a card but no media")
			return Media{}, err
		}
		err = fmt.Errorf("x embed: %w", ErrNoVideo)
		return Media{}, err
	}

	span.SetAttributes(attribute.Int("extractor.urls", len(urls)))
	return Media{URLs: urls, Metadata: tweet.metadata()}, nil
}

func (ex *XExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "x_metadata")
	defer tr.End(span, &err)

	tweet, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return tweet.metadata(), nil
}
