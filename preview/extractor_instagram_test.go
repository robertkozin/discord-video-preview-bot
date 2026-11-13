package preview

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInstagramShortcode(t *testing.T) {
	tests := map[string]string{
		"https://www.instagram.com/reel/Dfp6E98Bjxr/?igsh=x":   "Dfp6E98Bjxr",
		"https://instagram.com/reels/Dfp6E98Bjxr":              "Dfp6E98Bjxr",
		"https://www.instagram.com/p/DfpiweAilxF/":             "DfpiweAilxF",
		"https://www.instagram.com/ryu____1/reel/DfoqbOGOOUY/": "DfoqbOGOOUY",
		"https://www.instagram.com/tv/DfpiweAilxF/":            "DfpiweAilxF",
		"https://www.instagram.com/someone/":                   "",
		"https://www.instagram.com/stories/someone/123/":       "",
		"https://www.notinstagram.com/p/DfpiweAilxF/":          "",
	}
	for in, want := range tests {
		if got, _ := instagramPostCode(in); got != want {
			t.Errorf("instagramPostCode(%q) = %q, want %q", in, got, want)
		}
	}
	if id, _ := instagramMediaID("Dfp6E98Bjxr"); id != "4029006769740463211" {
		t.Errorf("media id = %s", id)
	}
	// longer shortcodes: only the first 11 characters are the id
	if id, _ := instagramMediaID("Dfp6E98BjxrAbCdEf"); id != "4029006769740463211" {
		t.Errorf("media id with suffix = %s", id)
	}
}

func newFakeInstagram(t *testing.T) *InstagramExtractor {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sessionid=ok") || r.Header.Get("X-IG-App-ID") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"login_required","require_login":true,"status":"fail"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/media/4029006769740463211/info/": // Dfp6E98Bjxr: a reel
			fmt.Fprint(w, `{"items":[{"media_type":2,"taken_at":1794515272,"like_count":5,"comment_count":2,"play_count":99,
				"caption":{"text":"hello"},"user":{"username":"someone","full_name":"Some One"},
				"video_versions":[{"url":"https://cdn/1080","width":1080,"height":1920},{"url":"https://cdn/720","width":720,"height":1280},{"url":"https://cdn/480","width":480,"height":854}]}],"status":"ok"}`)
		case "/api/v1/media/4028904205997202501/info/": // DfpiweAilxF: a carousel of a photo and a video
			fmt.Fprint(w, `{"items":[{"media_type":8,"user":{"username":"c"},"carousel_media":[
				{"image_versions2":{"candidates":[{"url":"https://cdn/photo-big","width":1440,"height":1440},{"url":"https://cdn/photo-small","width":320,"height":320}]}},
				{"image_versions2":{"candidates":[{"url":"https://cdn/thumb"}]},"video_versions":[{"url":"https://cdn/clip","width":720,"height":720}]}]}],"status":"ok"}`)
		case "/api/v1/media/0/info/":
			w.WriteHeader(http.StatusBadRequest) // as Instagram does, not 404
			fmt.Fprint(w, `{"message":"Invalid media_id 0","status":"fail"}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"Media not found or unavailable","status":"fail"}`)
		}
	}))
	t.Cleanup(srv.Close)
	config, _ := url.Parse("instagram://" + strings.TrimPrefix(srv.URL, "http://") + "?insecure=1&cookie=sessionid%3Dok")
	ex, err := NewInstagram(config)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func TestInstagramExtract(t *testing.T) {
	ex := newFakeInstagram(t)
	ctx := context.Background()

	media, err := ex.ExtractMedia(ctx, "https://www.instagram.com/reel/Dfp6E98Bjxr/")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(media.URLs) != "[https://cdn/720]" {
		t.Errorf("urls = %v", media.URLs)
	}
	m := media.Metadata
	if m.Description != "hello" || m.Author != "Some One" || m.AuthorHandle != "someone" || m.Likes != 5 || m.Views != 99 ||
		m.Comments != 2 || m.Source != "instagram" || !m.PostedAt.Equal(time.Unix(1794515272, 0)) {
		t.Errorf("metadata = %+v", m)
	}

	media, err = ex.ExtractMedia(ctx, "https://www.instagram.com/p/DfpiweAilxF/")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(media.URLs) != "[https://cdn/photo-big https://cdn/clip]" {
		t.Errorf("carousel urls = %v", media.URLs)
	}

	// gone: ErrNoVideo
	if _, err := ex.ExtractMedia(ctx, "https://www.instagram.com/p/Dfp6E98Bjxq/"); !errors.Is(err, ErrNoVideo) {
		t.Errorf("want ErrNoVideo, got %v", err)
	}
	// a shortcode that decodes to a bad id: Instagram answers 400 "Invalid media_id", also gone
	if _, err := ex.ExtractMedia(ctx, "https://www.instagram.com/p/AAAAAAAAAAA/"); !errors.Is(err, ErrNoVideo) {
		t.Errorf("want ErrNoVideo, got %v", err)
	}
	// dead session: a plain error so the next extractor tries
	ex.Cookie = "sessionid=expired"
	if _, err := ex.ExtractMedia(ctx, "https://www.instagram.com/reel/Dfp6E98Bjxr/"); err == nil || errors.Is(err, ErrNoVideo) || !strings.Contains(err.Error(), "login required") {
		t.Errorf("want a plain login error, got %v", err)
	}
}

func TestInstagramCookieFile(t *testing.T) {
	path := t.TempDir() + "/cookies.json"
	os.WriteFile(path, []byte(`{"instagram":["sessionid=ok; csrftoken=x"],"reddit":["y"]}`), 0o600)
	ex, err := NewInstagram(&url.URL{Scheme: "instagram", RawQuery: "cookies=" + url.QueryEscape(path)})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := ex.cookie(); c != "sessionid=ok; csrftoken=x" {
		t.Errorf("cookie = %q", c)
	}
	if _, err := NewInstagram(&url.URL{Scheme: "instagram"}); err == nil {
		t.Error("want an error without a session")
	}
}

// TestInstagramLive extracts real posts, with the session from INSTAGRAM_COOKIES (a cookies.json path):
//
//	INSTAGRAM_LIVE=1 INSTAGRAM_COOKIES=… INSTAGRAM_LIVE_URLS=https://www.instagram.com/reel/…,… go test ./preview -run TestInstagramLive -v
func TestInstagramLive(t *testing.T) {
	if os.Getenv("INSTAGRAM_LIVE") == "" {
		t.Skip("set INSTAGRAM_LIVE=1 to call Instagram")
	}
	ex, err := NewInstagram(&url.URL{Scheme: "instagram", RawQuery: "cookies=" + url.QueryEscape(os.Getenv("INSTAGRAM_COOKIES"))})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range strings.Split(os.Getenv("INSTAGRAM_LIVE_URLS"), ",") {
		start := time.Now()
		media, err := ex.ExtractMedia(context.Background(), u)
		if err != nil {
			t.Logf("FAIL %s (%s): %v", u, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		var sizes []string
		for _, mu := range media.URLs {
			b, err := downloadHttp(context.Background(), mu)
			if err != nil {
				sizes = append(sizes, "download: "+err.Error())
				continue
			}
			sizes = append(sizes, fmt.Sprintf("%s %dKB", detectContentType(b), len(b)/1024))
		}
		t.Logf("OK %s (%s): %v %q by @%s likes=%d views=%d", u, time.Since(start).Round(time.Millisecond), sizes, truncate(media.Metadata.Description, 40), media.Metadata.AuthorHandle, media.Metadata.Likes, media.Metadata.Views)
	}
}
