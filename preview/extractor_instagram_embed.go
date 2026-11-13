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
	"regexp"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*InstagramEmbedExtractor)(nil)
	_ MetadataProvider = (*InstagramEmbedExtractor)(nil)
)

// InstagramEmbedExtractor reads public posts from Instagram's embed page
// (instagram.com/p/<code>/embed/captioned/), the one InstaFix scrapes, with no
// account. The page carries the post as a JSON string (`contextJSON`) with
// direct CDN urls. Instagram only serves it to non-browser clients: a browser
// User-Agent gets an error page while `curl/…` (or iframe-style headers) gets
// the data (tested 2026-11 from the Decodo and WARP exits). Posts that need a
// login, are restricted or have their video blocked in embeds have no media
// here, so this is a plain error and `instagram://` or the fixers still try.
//
//	instagramembed://
//	instagramembed://localhost:8080?insecure=1 (a test server)
type InstagramEmbedExtractor struct {
	Endpoint string // https://www.instagram.com by default
}

func NewInstagramEmbed(config *url.URL) (*InstagramEmbedExtractor, error) {
	ex := &InstagramEmbedExtractor{Endpoint: "https://www.instagram.com"}
	if config.Host != "" {
		scheme := "https"
		if config.Query().Has("insecure") {
			scheme = "http"
		}
		ex.Endpoint = scheme + "://" + config.Host
	}
	return ex, nil
}

func (ex *InstagramEmbedExtractor) String() string { return "instagram embed at " + ex.Endpoint }

func (ex *InstagramEmbedExtractor) IsSupported(mediaURL string) bool {
	_, err := instagramPostCode(mediaURL)
	return err == nil
}

var instagramContextJSON = regexp.MustCompile(`"contextJSON":("(?:[^"\\]|\\.)*")`)

type instagramEmbedMedia struct {
	Typename   string `json:"__typename"`
	IsVideo    bool   `json:"is_video"`
	VideoURL   string `json:"video_url"`
	DisplayURL string `json:"display_url"`
	Owner      struct {
		Username string `json:"username"`
		FullName string `json:"full_name"`
	} `json:"owner"`
	Caption struct {
		Edges []struct {
			Node struct {
				Text string `json:"text"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"edge_media_to_caption"`
	VideoViewCount int64 `json:"video_view_count"`
	LikeCount      struct {
		Count int64 `json:"count"`
	} `json:"edge_liked_by"`
	TakenAt int64 `json:"taken_at_timestamp"`
	Sidecar struct {
		Edges []struct {
			Node instagramEmbedMedia `json:"node"`
		} `json:"edges"`
	} `json:"edge_sidecar_to_children"`
}

// parseInstagramEmbed finds the post in an embed page.
func parseInstagramEmbed(page []byte) (*instagramEmbedMedia, error) {
	m := instagramContextJSON.FindSubmatch(page)
	if m == nil {
		if strings.Contains(string(page), "WatchOnInstagram") {
			return nil, errors.New("instagram embed: video not available in embeds")
		}
		return nil, errors.New("instagram embed: no post data in the page (blocked, private or removed)")
	}
	var contextJSON string
	if err := json.Unmarshal(m[1], &contextJSON); err != nil {
		return nil, fmt.Errorf("instagram embed: reading contextJSON: %w", err)
	}
	var ctx struct {
		GQL struct {
			Media *instagramEmbedMedia `json:"shortcode_media"`
		} `json:"gql_data"`
	}
	if err := json.Unmarshal([]byte(contextJSON), &ctx); err != nil {
		return nil, fmt.Errorf("instagram embed: parsing contextJSON: %w", err)
	}
	if ctx.GQL.Media == nil {
		return nil, errors.New("instagram embed: no shortcode_media in the page")
	}
	return ctx.GQL.Media, nil
}

func (ex *InstagramEmbedExtractor) fetch(ctx context.Context, mediaURL string) (*instagramEmbedMedia, error) {
	code, err := instagramPostCode(mediaURL)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ex.Endpoint+"/p/"+code+"/embed/captioned/", nil)
	// a browser User-Agent gets an error page instead of the post
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := httpDo(req)
	if err != nil {
		return nil, fmt.Errorf("fetching instagram embed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading instagram embed: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("instagram embed: %s", resp.Status)
	}
	return parseInstagramEmbed(body)
}

func (m *instagramEmbedMedia) urls() []string {
	items := []instagramEmbedMedia{*m}
	if len(m.Sidecar.Edges) > 0 {
		items = items[:0]
		for _, e := range m.Sidecar.Edges {
			items = append(items, e.Node)
		}
	}
	var urls []string
	for _, it := range items {
		if len(urls) == instagramMaxItems {
			break
		}
		if u := cmp.Or(it.VideoURL, it.DisplayURL); u != "" {
			urls = append(urls, u)
		}
	}
	return urls
}

func (m *instagramEmbedMedia) metadata() *Metadata {
	md := &Metadata{
		Author:       m.Owner.FullName,
		AuthorHandle: m.Owner.Username,
		Views:        m.VideoViewCount,
		Likes:        m.LikeCount.Count,
		Source:       "instagram",
	}
	if len(m.Caption.Edges) > 0 {
		md.Description = strings.TrimSpace(m.Caption.Edges[0].Node.Text)
	}
	if m.TakenAt > 0 {
		md.PostedAt = time.Unix(m.TakenAt, 0).UTC()
	}
	return md
}

func (ex *InstagramEmbedExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *InstagramEmbedExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "instagram_embed_extract")
	defer EndExtractor(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}
	urls := post.urls()
	if len(urls) == 0 {
		err = errors.New("instagram embed: no media in post")
		return Media{}, err
	}
	span.SetAttributes(attribute.Int("extractor.urls", len(urls)), attribute.String("instagram.typename", post.Typename))
	return Media{URLs: urls, Metadata: post.metadata()}, nil
}

func (ex *InstagramEmbedExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "instagram_embed_metadata")
	defer tr.End(span, &err)

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return post.metadata(), nil
}
