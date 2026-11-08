package preview

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*RedditExtractor)(nil)
	_ MetadataProvider = (*RedditExtractor)(nil)
)

// RedditUserAgent is what Reddit's API rules ask clients to identify as.
const RedditUserAgent = "linux:discord-video-preview:v1 (homelab)"

// redditMaxShortSide caps a video's shorter side, so portrait videos aren't
// held to their 1920 height. Same 720p preference as the yt-dlp extractor.
const redditMaxShortSide = 720

// redditClient has no cookie jar, so API calls carry only the bearer token.
var redditClient = &http.Client{Timeout: 20 * time.Second}

// RedditAuth keeps an OAuth access token for Reddit's API, from
// REDDIT_CLIENT_ID, REDDIT_CLIENT_SECRET and REDDIT_REFRESH_TOKEN.
type RedditAuth struct {
	TokenURL string

	clientID, clientSecret, refreshToken string

	mu          sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

func NewRedditAuthFromEnv() (*RedditAuth, error) {
	a := &RedditAuth{
		TokenURL:     "https://www.reddit.com/api/v1/access_token",
		clientID:     os.Getenv("REDDIT_CLIENT_ID"),
		clientSecret: os.Getenv("REDDIT_CLIENT_SECRET"),
		refreshToken: os.Getenv("REDDIT_REFRESH_TOKEN"),
	}
	if a.clientID == "" || a.clientSecret == "" || a.refreshToken == "" {
		return nil, fmt.Errorf("reddit needs REDDIT_CLIENT_ID, REDDIT_CLIENT_SECRET and REDDIT_REFRESH_TOKEN")
	}
	return a, nil
}

// Token returns a current access token, refreshing it when it's near expiry.
func (a *RedditAuth) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accessToken != "" && time.Until(a.tokenExpiry) > 5*time.Minute {
		return a.accessToken, nil
	}

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.refreshToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(a.clientID, a.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", RedditUserAgent)

	resp, err := httpDoWith(redditClient, req)
	if err != nil {
		return "", fmt.Errorf("refreshing reddit token: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, megaByte)).Decode(&tok); err != nil {
		return "", fmt.Errorf("refreshing reddit token: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", fmt.Errorf("refreshing reddit token: %s %s", resp.Status, tok.Error)
	}
	a.accessToken = tok.AccessToken
	a.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return a.accessToken, nil
}

// RedditExtractor reads Reddit-hosted videos through the OAuth API and takes the
// tracks straight from v.redd.it, so it needs no proxy or browser cookies.
// Reddit serves video and audio separately, so they are merged with ffmpeg.
// Images, galleries and links to other sites are left to the other extractors.
type RedditExtractor struct {
	Endpoint string // https://oauth.reddit.com

	auth *RedditAuth
	mux  func(ctx context.Context, inputs ...string) (string, error)
}

func NewReddit(config *url.URL) (*RedditExtractor, error) {
	auth, err := NewRedditAuthFromEnv()
	if err != nil {
		return nil, err
	}
	return &RedditExtractor{Endpoint: "https://oauth.reddit.com", auth: auth, mux: muxToMP4}, nil
}

func (ex *RedditExtractor) String() string { return "reddit api at " + ex.Endpoint }

func (ex *RedditExtractor) IsSupported(mediaURL string) bool {
	_, err := redditLookup(mediaURL)
	return err == nil
}

var redditID = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// redditLookup returns the /api/info query that finds the post behind a url.
// A v.redd.it url is looked up by link, which also finds its crossposts.
func redditLookup(mediaURL string) (url.Values, error) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	var id string
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "v.redd.it":
		if id = parts[0]; redditID.MatchString(id) {
			return url.Values{"url": {"https://v.redd.it/" + id}}, nil
		}
	case "redd.it":
		id = parts[0]
	case "reddit.com", "old.reddit.com", "new.reddit.com", "sh.reddit.com":
		for i := 0; i+1 < len(parts); i++ {
			if parts[i] == "comments" {
				id = parts[i+1]
				break
			}
		}
	}
	if !redditID.MatchString(id) {
		return nil, fmt.Errorf("not a reddit post url: %s", mediaURL)
	}
	return url.Values{"id": {"t3_" + id}}, nil
}

type redditVideo struct {
	FallbackURL string `json:"fallback_url"`
	DashURL     string `json:"dash_url"`
	HasAudio    bool   `json:"has_audio"`
}

type redditMedia struct {
	RedditVideo *redditVideo `json:"reddit_video"`
}

type redditPost struct {
	Title       string       `json:"title"`
	Selftext    string       `json:"selftext"`
	Author      string       `json:"author"`
	Score       int64        `json:"score"`
	NumComments int64        `json:"num_comments"`
	CreatedUTC  float64      `json:"created_utc"`
	SecureMedia *redditMedia `json:"secure_media"`
	Media       *redditMedia `json:"media"`

	CrosspostParents []redditPost `json:"crosspost_parent_list"`
}

// video returns the post's hosted video, or that of the post it crossposts.
func (p *redditPost) video() *redditVideo {
	for _, m := range []*redditMedia{p.SecureMedia, p.Media} {
		if m != nil && m.RedditVideo != nil {
			return m.RedditVideo
		}
	}
	for i := range p.CrosspostParents {
		if v := p.CrosspostParents[i].video(); v != nil {
			return v
		}
	}
	return nil
}

func (p *redditPost) metadata() *Metadata {
	m := &Metadata{
		Title:       p.Title,
		Description: p.Selftext,
		Likes:       p.Score,
		Comments:    p.NumComments,
		Source:      "reddit",
	}
	if p.Author != "[deleted]" {
		m.AuthorHandle = p.Author
	}
	if p.CreatedUTC > 0 {
		m.PostedAt = time.Unix(int64(p.CreatedUTC), 0).UTC()
	}
	return m
}

// fetch finds the post, preferring one with a video when a link has several.
func (ex *RedditExtractor) fetch(ctx context.Context, mediaURL string) (*redditPost, error) {
	lookup, err := redditLookup(mediaURL)
	if err != nil {
		return nil, err
	}
	lookup.Set("raw_json", "1")

	token, err := ex.auth.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ex.Endpoint+"/api/info?"+lookup.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", RedditUserAgent)

	resp, err := httpDoWith(redditClient, req)
	if err != nil {
		return nil, fmt.Errorf("fetching reddit post: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading reddit post: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching reddit post: %s: %s", resp.Status, truncate(string(body), 120))
	}

	var listing struct {
		Data struct {
			Children []struct {
				Data redditPost `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("parsing reddit post: %w", err)
	}
	children := listing.Data.Children
	if len(children) == 0 {
		return nil, fmt.Errorf("reddit: post not found: %s", mediaURL)
	}
	for i := range children {
		if children[i].Data.video() != nil {
			return &children[i].Data, nil
		}
	}
	return &children[0].Data, nil
}

func (ex *RedditExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *RedditExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "reddit_extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return Media{}, err
	}
	video := post.video()
	if video == nil {
		err = fmt.Errorf("reddit: post has no hosted video")
		return Media{}, err
	}
	span.SetAttributes(attribute.Bool("reddit.has_audio", video.HasAudio))

	videoURL, audioURL, err := ex.tracks(ctx, video)
	if err != nil {
		return Media{}, err
	}
	if audioURL == "" {
		return Media{URLs: []string{videoURL}, Metadata: post.metadata()}, nil
	}
	merged, err := ex.mux(ctx, videoURL, audioURL)
	if err != nil {
		return Media{}, err
	}
	return Media{URLs: []string{merged}, Metadata: post.metadata()}, nil
}

func (ex *RedditExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "reddit_metadata")
	defer tr.End(span, &err)

	post, err := ex.fetch(ctx, mediaURL)
	if err != nil {
		return nil, err
	}
	return post.metadata(), nil
}

// tracks picks the urls to download. A silent video can make do with the
// fallback url when the manifest is unavailable, but one with sound can't: the
// fallback has no audio, and a mute video shouldn't count as a success.
func (ex *RedditExtractor) tracks(ctx context.Context, video *redditVideo) (videoURL, audioURL string, err error) {
	videoURL, audioURL, err = ex.dashTracks(ctx, video.DashURL)
	switch {
	case err == nil && video.HasAudio && audioURL == "":
		return "", "", fmt.Errorf("reddit: manifest has no audio track for a video with sound")
	case err == nil:
		return videoURL, audioURL, nil
	case !video.HasAudio && video.FallbackURL != "":
		traceSpan(ctx).AddEvent("reddit_manifest_failed")
		return video.FallbackURL, "", nil
	}
	return "", "", err
}

func (ex *RedditExtractor) dashTracks(ctx context.Context, dashURL string) (videoURL, audioURL string, err error) {
	if dashURL == "" {
		return "", "", fmt.Errorf("reddit: video has no manifest")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dashURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := httpDoWith(redditClient, req)
	if err != nil {
		return "", "", fmt.Errorf("fetching reddit manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetching reddit manifest: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, megaByte))
	if err != nil {
		return "", "", fmt.Errorf("reading reddit manifest: %w", err)
	}
	base, err := url.Parse(dashURL)
	if err != nil {
		return "", "", err
	}
	return pickRedditTracks(body, base)
}

type redditManifest struct {
	Periods []struct {
		Sets []struct {
			ContentType string `xml:"contentType,attr"`
			MimeType    string `xml:"mimeType,attr"`
			Reps        []struct {
				MimeType  string `xml:"mimeType,attr"`
				Codecs    string `xml:"codecs,attr"`
				Bandwidth int64  `xml:"bandwidth,attr"`
				Width     int    `xml:"width,attr"`
				Height    int    `xml:"height,attr"`
				BaseURL   string `xml:"BaseURL"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// pickRedditTracks chooses the best h264 video within redditMaxShortSide (or
// the smallest one if none fit) and the best audio. audio is "" if there is
// none. Older posts don't set contentType, so the mime type is checked too.
func pickRedditTracks(manifest []byte, base *url.URL) (video, audio string, err error) {
	var mpd redditManifest
	if err := xml.Unmarshal(manifest, &mpd); err != nil {
		return "", "", fmt.Errorf("parsing reddit manifest: %w", err)
	}

	var bestVideo, bestAudio string
	var videoRate, audioRate int64
	videoFits := false
	for _, period := range mpd.Periods {
		for _, set := range period.Sets {
			for _, rep := range set.Reps {
				if rep.BaseURL == "" {
					continue
				}
				kind := set.ContentType + " " + set.MimeType + " " + rep.MimeType
				switch {
				case strings.Contains(kind, "video"):
					if rep.Codecs != "" && !strings.HasPrefix(rep.Codecs, "avc1") {
						continue
					}
					fits := min(rep.Width, rep.Height) <= redditMaxShortSide
					better := bestVideo == "" || (fits && !videoFits) ||
						(fits == videoFits && fits && rep.Bandwidth > videoRate) ||
						(fits == videoFits && !fits && rep.Bandwidth < videoRate)
					if better {
						bestVideo, videoRate, videoFits = rep.BaseURL, rep.Bandwidth, fits
					}
				case strings.Contains(kind, "audio"):
					if bestAudio == "" || rep.Bandwidth > audioRate {
						bestAudio, audioRate = rep.BaseURL, rep.Bandwidth
					}
				}
			}
		}
	}
	if bestVideo == "" {
		return "", "", fmt.Errorf("reddit: manifest has no usable video track")
	}

	videoURL, err := base.Parse(bestVideo)
	if err != nil {
		return "", "", fmt.Errorf("reddit: bad video track %q: %w", bestVideo, err)
	}
	if bestAudio == "" {
		return videoURL.String(), "", nil
	}
	audioURL, err := base.Parse(bestAudio)
	if err != nil {
		return "", "", fmt.Errorf("reddit: bad audio track %q: %w", bestAudio, err)
	}
	return videoURL.String(), audioURL.String(), nil
}
