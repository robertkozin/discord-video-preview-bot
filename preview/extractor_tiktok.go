package preview

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var (
	_ MediaExtractor   = (*TikTokExtractor)(nil)
	_ MetadataProvider = (*TikTokExtractor)(nil)
)

// TikTokExtractor reads a post straight from its TikTok web page, which embeds
// the post as JSON (__UNIVERSAL_DATA_FOR_REHYDRATION__), and downloads the
// media itself: TikTok's media urls only answer with the cookies the page
// set, a tiktok.com Referer and the same User-Agent, so each extraction gets
// its own session and hands the reuploader file:// urls.
type TikTokExtractor struct {
	// Endpoint is the site, https://www.tiktok.com by default.
	Endpoint string
}

func NewTikTok(config *url.URL) (*TikTokExtractor, error) {
	endpoint := "https://www.tiktok.com"
	// tiktok://host:port?insecure=1 points elsewhere, e.g. a test server
	if config.Host != "" {
		scheme := "https"
		if config.Query().Has("insecure") {
			scheme = "http"
		}
		endpoint = scheme + "://" + config.Host
	}
	return &TikTokExtractor{Endpoint: endpoint}, nil
}

func (ex *TikTokExtractor) String() string {
	return "tiktok at " + ex.Endpoint
}

func (ex *TikTokExtractor) IsSupported(mediaURL string) bool {
	_, ok := tikTokPostID(mediaURL)
	return ok
}

// tikTokPostID returns the post id of a TikTok post url:
// /@user/video/<id>, /@user/photo/<id>, /embed/v2/<id>, /share/video/<id>, or
// m.tiktok.com/v/<id>.html. Share links (vm.tiktok.com/…) are resolved to one
// of these before the extractors run, see resolveTikTokShortLink.
func tikTokPostID(mediaURL string) (string, bool) {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "tiktok.com" && !strings.HasSuffix(host, ".tiktok.com") {
		return "", false
	}
	segs := pathSegments(u.Path)
	var id string
	switch {
	case len(segs) >= 3 && strings.HasPrefix(segs[0], "@") && (segs[1] == "video" || segs[1] == "photo"):
		id = segs[2]
	case len(segs) >= 3 && segs[0] == "share" && segs[1] == "video":
		id = segs[2]
	case len(segs) >= 2 && segs[0] == "embed":
		id = segs[len(segs)-1]
	case len(segs) == 2 && segs[0] == "v":
		id = strings.TrimSuffix(segs[1], ".html")
	}
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", false
	}
	return id, true
}

type tikTokItem struct {
	ID                  string  `json:"id"`
	Desc                string  `json:"desc"`
	CreateTime          flexInt `json:"createTime"`
	IsContentClassified bool    `json:"isContentClassified"` // age-restricted, needs a login
	Author              struct {
		UniqueID string `json:"uniqueId"`
		Nickname string `json:"nickname"`
	} `json:"author"`
	Stats struct {
		DiggCount    flexInt `json:"diggCount"`
		ShareCount   flexInt `json:"shareCount"`
		CommentCount flexInt `json:"commentCount"`
		PlayCount    flexInt `json:"playCount"`
	} `json:"stats"`
	Video struct {
		PlayAddr    string          `json:"playAddr"`
		BitrateInfo []tikTokBitrate `json:"bitrateInfo"`
	} `json:"video"`
	ImagePost *struct {
		Images []struct {
			ImageURL struct {
				URLList []string `json:"urlList"`
			} `json:"imageURL"`
		} `json:"images"`
	} `json:"imagePost"`
}

type tikTokBitrate struct {
	GearName  string `json:"GearName"`
	Bitrate   int64  `json:"Bitrate"`
	CodecType string `json:"CodecType"` // h264, h265_hvc1, bytevc2 (unplayable)…
	PlayAddr  struct {
		Width   int      `json:"Width"`
		Height  int      `json:"Height"`
		URLList []string `json:"UrlList"`
	} `json:"PlayAddr"`
}

// flexInt reads a JSON number or a number in a string; TikTok uses both, even
// within one object.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil // a stat isn't worth failing the post over
	}
	*n = flexInt(v)
	return nil
}

func (it *tikTokItem) metadata() *Metadata {
	m := &Metadata{
		Description:  it.Desc,
		Author:       it.Author.Nickname,
		AuthorHandle: it.Author.UniqueID,
		Likes:        int64(it.Stats.DiggCount),
		Views:        int64(it.Stats.PlayCount),
		Comments:     int64(it.Stats.CommentCount),
		Shares:       int64(it.Stats.ShareCount),
		Source:       "tiktok",
	}
	if it.CreateTime > 0 {
		m.PostedAt = time.Unix(int64(it.CreateTime), 0).UTC()
	}
	return m
}

// tikTokSession is one visit to TikTok: the page's cookies, sent back when
// downloading its media.
type tikTokSession struct {
	client  *http.Client
	referer string
}

var tikTokUniversalData = regexp.MustCompile(`(?s)<script[^>]+id="__UNIVERSAL_DATA_FOR_REHYDRATION__"[^>]*>(.*?)</script>`)

// fetch loads the post's page and returns its item. A page without the post's
// JSON may be TikTok's proof-of-work challenge, which is solved once.
func (ex *TikTokExtractor) fetch(ctx context.Context, id string) (*tikTokItem, *tikTokSession, error) {
	span := traceSpan(ctx)
	// the user in the path doesn't matter, and /video/ also serves photo posts
	session := &tikTokSession{client: newSessionClient(), referer: ex.Endpoint + "/@i/video/" + id}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	page, err := session.get(ctx, session.referer)
	if err != nil {
		return nil, nil, err
	}
	data := tikTokUniversalData.FindSubmatch(page)
	if data == nil {
		solved, err := solveTikTokChallenge(page, session.client.Jar, ex.Endpoint)
		if err != nil {
			return nil, nil, fmt.Errorf("tiktok: page has no post data: %w", err)
		}
		span.AddEvent("challenge_solved")
		page, err = session.get(ctx, session.referer)
		if err != nil {
			return nil, nil, err
		}
		// the challenge cookies are single use
		expireCookies(session.client.Jar, ex.Endpoint, solved...)
		if data = tikTokUniversalData.FindSubmatch(page); data == nil {
			return nil, nil, fmt.Errorf("tiktok: page has no post data after the challenge: %s", truncate(string(page), 120))
		}
	}

	var universal struct {
		Scope struct {
			VideoDetail *struct {
				StatusCode int    `json:"statusCode"`
				StatusMsg  string `json:"statusMsg"`
				ItemInfo   struct {
					ItemStruct *tikTokItem `json:"itemStruct"`
				} `json:"itemInfo"`
			} `json:"webapp.video-detail"`
		} `json:"__DEFAULT_SCOPE__"`
	}
	if err := json.Unmarshal(data[1], &universal); err != nil {
		return nil, nil, fmt.Errorf("parsing tiktok post data: %w", err)
	}
	detail := universal.Scope.VideoDetail
	if detail == nil {
		return nil, nil, errors.New("tiktok: page has no webapp.video-detail")
	}
	span.SetAttributes(attribute.Int("tiktok.status", detail.StatusCode))
	item := detail.ItemInfo.ItemStruct
	span.SetAttributes(attribute.String("tiktok.status_msg", detail.StatusMsg))
	switch {
	case tikTokDeleted(detail.StatusCode, detail.StatusMsg):
		// TikTok says the post or its author is gone: nobody can get it
		return nil, nil, fmt.Errorf("tiktok: post deleted (status %d %s): %w", detail.StatusCode, detail.StatusMsg, ErrNoVideo)
	case detail.StatusCode != 0 || item == nil || item.ID == "":
		// plain errors: a region lock (10231), a block of this IP (10204) or a
		// private post (10216, 10222, 10204 status_self_see) may still work for
		// another extractor
		return nil, nil, fmt.Errorf("tiktok: post unavailable: status %d %s (%s)", detail.StatusCode, detail.StatusMsg, tikTokStatusText(detail.StatusCode))
	case item.IsContentClassified && item.Video.PlayAddr == "" && item.ImagePost == nil:
		return nil, nil, errors.New("tiktok: age-restricted post needs a login")
	}
	return item, session, nil
}

// tikTokDeleted reports whether a status says the post no longer exists. 10204
// is also what a blocked IP or a post only its author can see
// (status_self_see) gets, so it goes by the message.
func tikTokDeleted(status int, msg string) bool {
	if status != 10204 {
		return false
	}
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "status_deleted") || strings.Contains(msg, "doesn't exist") || strings.Contains(msg, "author_invalid")
}

func tikTokStatusText(status int) string {
	switch status {
	case 10204:
		return "not found, or this IP is blocked"
	case 10216, 10222:
		return "private"
	case 10231:
		return "not available in this region"
	}
	return "unknown"
}

func (s *tikTokSession) get(ctx context.Context, pageURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := httpDoWith(s.client, req)
	if err != nil {
		return nil, fmt.Errorf("fetching tiktok page: %w", err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Path == "/login" {
		return nil, errors.New("tiktok: redirected to login")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*megaByte))
	if err != nil {
		return nil, fmt.Errorf("reading tiktok page: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tiktok page: %s: %s", resp.Status, truncate(string(body), 120))
	}
	return body, nil
}

// download fetches one media url with the session's cookies into a temp file
// and returns its file:// url.
func (s *tikTokSession) download(ctx context.Context, mediaURL, ext string) (string, error) {
	header := http.Header{"Referer": {s.referer}}
	body, err := downloadHttpWith(ctx, s.client, mediaURL, header)
	if err != nil {
		return "", err
	}
	path, err := newTempMediaFile("tiktok-*" + ext)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("writing tiktok media: %w", err)
	}
	return "file://" + path, nil
}

// downloadFirst tries each mirror of one media until one works.
func (s *tikTokSession) downloadFirst(ctx context.Context, mirrors []string, ext string) (string, error) {
	var errs []error
	for _, u := range mirrors {
		file, err := s.download(ctx, u, ext)
		if err == nil {
			return file, nil
		}
		errs = append(errs, err)
		if ctx.Err() != nil || errors.Is(err, errDownloadStalled) || errors.Is(err, errMediaTooLarge) {
			break // another mirror won't do better
		}
	}
	return "", fmt.Errorf("downloading tiktok media: %w", errors.Join(errs...))
}

// pickTikTokVideo returns the mirrors of the best h264 rendition whose short
// side is ≤720, or the smallest h264 one when they're all bigger, or the
// page's default playAddr when there's no h264 rendition. h265 and TikTok's
// own bytevc2 don't play everywhere in Discord.
func pickTikTokVideo(it *tikTokItem) []string {
	var h264 []tikTokBitrate
	for _, b := range it.Video.BitrateInfo {
		if b.CodecType == "h264" && len(b.PlayAddr.URLList) > 0 {
			h264 = append(h264, b)
		}
	}
	slices.SortFunc(h264, func(a, b tikTokBitrate) int { return cmp.Compare(b.Bitrate, a.Bitrate) })
	for _, b := range h264 {
		if short := min(b.PlayAddr.Width, b.PlayAddr.Height); short == 0 || short <= 720 {
			return b.PlayAddr.URLList
		}
	}
	if len(h264) > 0 {
		return h264[len(h264)-1].PlayAddr.URLList
	}
	if it.Video.PlayAddr != "" {
		return []string{it.Video.PlayAddr}
	}
	return nil
}

// tikTokImage picks a jpeg among an image's mirrors (others are webp or heic,
// which Discord or the reuploader can't use).
func tikTokImage(mirrors []string) string {
	for _, u := range mirrors {
		if strings.Contains(u, ".jpeg?") || strings.Contains(u, ".jpg?") {
			return u
		}
	}
	return ""
}

func (ex *TikTokExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *TikTokExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "tiktok_extract")
	defer EndSpan(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	id, ok := tikTokPostID(mediaURL)
	if !ok {
		return Media{}, fmt.Errorf("not a tiktok post url: %s", mediaURL)
	}
	item, session, err := ex.fetch(ctx, id)
	if err != nil {
		return Media{}, err
	}

	var files []string
	defer func() {
		if err != nil {
			for _, f := range files {
				os.Remove(strings.TrimPrefix(f, "file://"))
			}
		}
	}()

	if item.ImagePost != nil && len(item.ImagePost.Images) > 0 {
		span.SetAttributes(attribute.Bool("tiktok.photo", true), attribute.Int("tiktok.images", len(item.ImagePost.Images)))
		for i, img := range item.ImagePost.Images {
			u := tikTokImage(img.ImageURL.URLList)
			if u == "" {
				return Media{}, fmt.Errorf("tiktok: no jpeg for image %d", i+1)
			}
			file, err := session.downloadFirst(ctx, []string{u}, ".jpg")
			if err != nil {
				return Media{}, err
			}
			files = append(files, file)
		}
	} else {
		mirrors := pickTikTokVideo(item)
		if len(mirrors) == 0 {
			return Media{}, errors.New("tiktok: post has no playable video")
		}
		file, err := session.downloadFirst(ctx, mirrors, ".mp4")
		if err != nil {
			return Media{}, err
		}
		files = append(files, file)
	}

	span.SetAttributes(attribute.Int("extractor.urls", len(files)))
	return Media{URLs: files, Metadata: item.metadata()}, nil
}

func (ex *TikTokExtractor) Metadata(ctx context.Context, mediaURL string) (_ *Metadata, err error) {
	ctx, span := tracer.Start(ctx, "tiktok_metadata")
	defer tr.End(span, &err)

	id, ok := tikTokPostID(mediaURL)
	if !ok {
		return nil, fmt.Errorf("not a tiktok post url: %s", mediaURL)
	}
	item, _, err := ex.fetch(ctx, id)
	if err != nil {
		return nil, err
	}
	return item.metadata(), nil
}

var (
	tikTokChallengeAttr = func(id string) *regexp.Regexp {
		return regexp.MustCompile(`<[^>]+\bid="` + id + `"[^>]*\bclass="([^"]*)"|<[^>]+\bclass="([^"]*)"[^>]*\bid="` + id + `"`)
	}
	tikTokChallengeData   = tikTokChallengeAttr("cs")
	tikTokChallengeCookie = tikTokChallengeAttr("wci")
	tikTokChallengeRCName = tikTokChallengeAttr("rci")
	tikTokChallengeRCVal  = tikTokChallengeAttr("rs")
)

func challengeAttr(re *regexp.Regexp, page []byte) string {
	m := re.FindSubmatch(page)
	if m == nil {
		return ""
	}
	return cmp.Or(string(m[1]), string(m[2]))
}

// solveTikTokChallenge solves the proof-of-work page TikTok sometimes serves
// instead of the post: find the number whose sha256 after a given prefix is a
// given digest, then set it as a cookie. It returns the cookie names it set.
// Ported from yt-dlp (TikTokBaseIE._solve_challenge_and_set_cookies).
func solveTikTokChallenge(page []byte, jar http.CookieJar, endpoint string) ([]string, error) {
	raw := challengeAttr(tikTokChallengeData, page)
	if raw == "" {
		if bytes.Contains(page, []byte("Please wait...")) {
			return nil, errors.New("challenge page without challenge data")
		}
		return nil, fmt.Errorf("unexpected page: %s", truncate(string(page), 120))
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
	if err != nil {
		return nil, fmt.Errorf("decoding challenge: %w", err)
	}
	var challenge map[string]any
	if err := json.Unmarshal(decoded, &challenge); err != nil {
		return nil, fmt.Errorf("parsing challenge: %w", err)
	}
	v, _ := challenge["v"].(map[string]any)
	a, _ := v["a"].(string)
	c, _ := v["c"].(string)
	prefix, errA := base64.StdEncoding.DecodeString(a)
	want, errC := base64.StdEncoding.DecodeString(c)
	if a == "" || c == "" || errA != nil || errC != nil {
		return nil, errors.New("challenge without prefix or digest")
	}

	answer := -1
	for i := 0; i <= 1_000_000; i++ {
		sum := sha256.Sum256(append(slices.Clip(prefix), strconv.Itoa(i)...))
		if bytes.Equal(sum[:], want) {
			answer = i
			break
		}
	}
	if answer < 0 {
		return nil, errors.New("challenge has no solution")
	}
	challenge["d"] = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(answer)))
	solved, _ := json.Marshal(challenge)

	name := challengeAttr(tikTokChallengeCookie, page)
	if name == "" {
		return nil, errors.New("challenge without cookie name")
	}
	u, _ := url.Parse(endpoint)
	cookies := []*http.Cookie{{Name: name, Value: base64.StdEncoding.EncodeToString(solved), Path: "/"}}
	if rcName, rcVal := challengeAttr(tikTokChallengeRCName, page), challengeAttr(tikTokChallengeRCVal, page); rcName != "" && rcVal != "" {
		cookies = append(cookies, &http.Cookie{Name: rcName, Value: rcVal, Path: "/"})
	}
	jar.SetCookies(u, cookies)

	names := make([]string, len(cookies))
	for i, c := range cookies {
		names[i] = c.Name
	}
	return names, nil
}

func expireCookies(jar http.CookieJar, endpoint string, names ...string) {
	u, _ := url.Parse(endpoint)
	var expired []*http.Cookie
	for _, n := range names {
		expired = append(expired, &http.Cookie{Name: n, Path: "/", MaxAge: -1})
	}
	jar.SetCookies(u, expired)
}
