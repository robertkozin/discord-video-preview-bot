package preview

import (
	"context"
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
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/html"
)

var (
	_ MediaExtractor   = (*InstaFixExtractor)(nil)
	_ MetadataProvider = (*InstaFixExtractor)(nil)
)

// InstaFixExtractor reads Instagram media from an "embed fixer": a site that
// mirrors instagram.com paths and answers with og:video / og:image tags for
// Discord, like InstaFix forks (eeinstagram.com, uuinstagram.com) or
// vxinstagram.com. The media urls redirect to Instagram's CDN, so they download
// like any other url. It is plain HTTP with no login, and the hosts come and go,
// so configure a couple of them in a row.
//
//	instafix://eeinstagram.com
//	instafix://vxinstagram.com?carousel=0
//	instafix://localhost:8080?insecure=1&carousel=0 (self-hosted, over http)
//
// With carousel on (the default) every item of a carousel is fetched from
// /p/<id>/1, /p/<id>/2, … until a page has no media, which is how InstaFix
// numbers them. carousel=0 takes only the post's first item, for hosts that
// answer past the end of a carousel (vxinstagram).
type InstaFixExtractor struct {
	Endpoint string // e.g. https://eeinstagram.com
	Host     string
	Carousel bool
}

// instaFixUserAgent is what the embed fixers serve their meta tags to.
const instaFixUserAgent = "Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)"

// instaFixMaxItems caps a carousel; a Discord media gallery holds 10.
const instaFixMaxItems = 10

// instaFixClient doesn't follow redirects: a fixer that can't get a post often
// redirects to instagram.com itself, which has no usable meta tags.
var instaFixClient = &http.Client{
	Transport:     http.DefaultTransport,
	Timeout:       30 * time.Second, // same as httpClient
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func NewInstaFix(config *url.URL) (*InstaFixExtractor, error) {
	if config.Host == "" {
		return nil, fmt.Errorf("instafix needs a host, e.g. instafix://eeinstagram.com")
	}
	q := config.Query()
	scheme := "https"
	if q.Has("insecure") {
		scheme = "http"
	}
	return &InstaFixExtractor{
		Endpoint: scheme + "://" + config.Host,
		Host:     config.Host,
		Carousel: q.Get("carousel") != "0",
	}, nil
}

func (ex *InstaFixExtractor) String() string { return "instafix at " + ex.Endpoint }

// Name tells this host apart from other instafix hosts in traces.
func (ex *InstaFixExtractor) Name() string { return "instafix:" + ex.Host }

func (ex *InstaFixExtractor) IsSupported(mediaURL string) bool {
	_, _, err := instagramPost(mediaURL)
	return err == nil
}

var instagramShortcode = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// instagramPost returns the kind ("p" or "reel") and shortcode of an Instagram
// post url, including the instagram.com/<user>/p/<id> form.
func instagramPost(mediaURL string) (kind, id string, err error) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", "", err
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "instagram.com" {
		return "", "", fmt.Errorf("not an instagram url: %s", mediaURL)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+1 < len(parts) && i < 2; i++ {
		switch parts[i] {
		case "p", "tv":
			kind = "p"
		case "reel", "reels":
			kind = "reel"
		default:
			continue
		}
		if id = parts[i+1]; instagramShortcode.MatchString(id) {
			return kind, id, nil
		}
	}
	return "", "", fmt.Errorf("not an instagram post url: %s", mediaURL)
}

// instaFixPage is what one page of a fixer said.
type instaFixPage struct {
	media    string // absolute url of the video, or else the image
	metadata *Metadata
}

func (ex *InstaFixExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *InstaFixExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "instafix_extract")
	defer EndExtractor(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)), attribute.String("instafix.host", ex.Host))

	kind, id, err := instagramPost(mediaURL)
	if err != nil {
		return Media{}, err
	}

	// a reel is never a carousel, so its own page is all there is
	if !ex.Carousel || kind == "reel" {
		page, err := ex.fetchPage(ctx, "/"+kind+"/"+id+"/")
		if err != nil {
			return Media{}, err
		}
		if page.media == "" {
			return Media{}, fmt.Errorf("instafix %s: no media for %s", ex.Host, id)
		}
		span.SetAttributes(attribute.Int("extractor.urls", 1))
		return Media{URLs: []string{page.media}, Metadata: page.metadata}, nil
	}

	var (
		urls     []string
		metadata *Metadata
	)
	for n := 1; n <= instaFixMaxItems; n++ {
		page, err := ex.fetchPage(ctx, fmt.Sprintf("/p/%s/%d", id, n))
		if err != nil {
			if n == 1 {
				return Media{}, err
			}
			// keep what we have rather than lose the whole post to one item
			span.AddEvent("instafix_item_failed", trace.WithAttributes(attribute.Int("item", n), attribute.String("error", truncate(err.Error(), 300))))
			break
		}
		if page.media == "" {
			break // past the last item
		}
		if metadata == nil {
			metadata = page.metadata
		}
		urls = append(urls, page.media)
	}
	if len(urls) == 0 {
		return Media{}, fmt.Errorf("instafix %s: no media for %s", ex.Host, id)
	}
	span.SetAttributes(attribute.Int("extractor.urls", len(urls)))
	return Media{URLs: urls, Metadata: metadata}, nil
}

func (ex *InstaFixExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "instafix_metadata")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("instafix.host", ex.Host))

	kind, id, err := instagramPost(mediaURL)
	if err != nil {
		return nil, err
	}
	page, err := ex.fetchPage(ctx, "/"+kind+"/"+id+"/")
	if err != nil {
		return nil, err
	}
	return page.metadata, nil
}

var errInstaFixRedirect = errors.New("redirected")

// fetchPage reads the meta tags of one page of the fixer. A page without media
// (past the end of a carousel) is not an error; a missing post or a redirect
// away from the fixer is.
func (ex *InstaFixExtractor) fetchPage(ctx context.Context, path string) (instaFixPage, error) {
	pageURL := ex.Endpoint + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return instaFixPage{}, err
	}
	req.Header.Set("User-Agent", instaFixUserAgent)

	resp, err := httpDoWith(instaFixClient, req)
	if err != nil {
		return instaFixPage{}, fmt.Errorf("fetching instafix %s: %w", ex.Host, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return instaFixPage{}, fmt.Errorf("instafix %s: %w to %s", ex.Host, errInstaFixRedirect, truncate(resp.Header.Get("Location"), 80))
	case resp.StatusCode != http.StatusOK:
		return instaFixPage{}, fmt.Errorf("instafix %s: %s", ex.Host, resp.Status)
	}

	meta, err := parseMetaTags(io.LimitReader(resp.Body, 2*megaByte))
	if err != nil {
		return instaFixPage{}, fmt.Errorf("reading instafix %s page: %w", ex.Host, err)
	}

	page := instaFixPage{metadata: instaFixMetadata(meta, ex.Name())}
	media := firstOf(meta, "og:video:secure_url", "og:video", "twitter:player:stream")
	if media == "" {
		media = firstOf(meta, "og:image", "twitter:image")
	}
	if media != "" {
		abs, err := resp.Request.URL.Parse(media) // relative on InstaFix, e.g. /videos/<id>/1
		if err != nil {
			return instaFixPage{}, fmt.Errorf("instafix %s: bad media url %q: %w", ex.Host, media, err)
		}
		page.media = abs.String()
	}
	return page, nil
}

// instaFixHandleTitle matches the "Name (@handle)" titles vxinstagram uses.
var instaFixHandleTitle = regexp.MustCompile(`^(.*?)\s*\(@([A-Za-z0-9._]+)\)$`)

// instaFixMetadata reads the author and caption out of a fixer's meta tags:
// InstaFix titles a post "@handle", vxinstagram "Name (@handle)".
func instaFixMetadata(meta map[string]string, source string) *Metadata {
	m := &Metadata{
		Description: strings.TrimSpace(firstOf(meta, "og:description", "twitter:description", "description")),
		Source:      source,
	}
	title := strings.TrimSpace(firstOf(meta, "og:title", "twitter:title"))
	if sm := instaFixHandleTitle.FindStringSubmatch(title); sm != nil {
		m.Author, m.AuthorHandle = sm[1], sm[2]
	} else if handle, ok := strings.CutPrefix(title, "@"); ok && !strings.ContainsAny(handle, " \t") {
		m.AuthorHandle = handle
	}
	if m.Empty() {
		return nil
	}
	return m
}

// parseMetaTags returns the content of every <meta property|name=... content=...>
// in an HTML document, keyed by lowercased property or name. The first
// occurrence of a key wins.
func parseMetaTags(r io.Reader) (map[string]string, error) {
	meta := map[string]string{}
	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) {
				return meta, nil
			}
			return meta, z.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) == "body" {
				return meta, nil // meta tags live in the head
			}
			if string(name) != "meta" || !hasAttr {
				continue
			}
			var key, content string
			for more := true; more; {
				var k, v []byte
				k, v, more = z.TagAttr()
				switch string(k) {
				case "property", "name":
					key = strings.ToLower(string(v))
				case "content":
					content = string(v)
				}
			}
			if _, seen := meta[key]; key != "" && !seen {
				meta[key] = content
			}
		}
	}
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}
