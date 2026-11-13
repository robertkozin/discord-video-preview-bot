package preview

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTikTokPostID(t *testing.T) {
	tests := map[string]string{
		"https://www.tiktok.com/@some.user/video/7321651079177866498?is_from_webapp=1": "7321651079177866498",
		"https://www.tiktok.com/@some_user/photo/7321651079177866498":                  "7321651079177866498",
		"https://m.tiktok.com/v/7321651079177866498.html":                              "7321651079177866498",
		"https://www.tiktok.com/embed/v2/7321651079177866498":                          "7321651079177866498",
		"https://www.tiktok.com/share/video/7321651079177866498":                       "7321651079177866498",
		"https://vm.tiktok.com/ZGdXagkJN/":                                             "",
		"https://www.tiktok.com/@some.user":                                            "",
		"https://www.tiktok.com/view/product/1732837595416859453":                      "",
		"https://www.nottiktok.com/@a/video/7321651079177866498":                       "",
	}
	for in, want := range tests {
		if got, _ := tikTokPostID(in); got != want {
			t.Errorf("tikTokPostID(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeTikTok serves post pages and media like TikTok: media only with the
// page's cookie and a Referer, and optionally a proof-of-work page first.
type fakeTikTok struct {
	srv       *httptest.Server
	items     map[string]string // id → itemStruct JSON (or "status:<code>")
	challenge atomic.Bool       // serve a challenge until it's solved
}

const fakeChallengePrefix = "prefix-bytes"
const fakeChallengeAnswer = 4242

func newFakeTikTok(t *testing.T, items map[string]string) *fakeTikTok {
	t.Helper()
	f := &fakeTikTok{items: items}
	mux := http.NewServeMux()
	mux.HandleFunc("/@i/video/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.challenge.Load() {
			if c, err := r.Cookie("_wafchallengeid"); err == nil && f.validAnswer(c.Value) {
				f.challenge.Store(false)
			} else {
				f.writeChallenge(w)
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: "tt_chain_token", Value: "session-" + r.PathValue("id"), Path: "/"})
		item := f.items[r.PathValue("id")]
		detail := `{"statusCode":0,"itemInfo":{"itemStruct":` + item + `}}`
		if status, ok := strings.CutPrefix(item, "status:"); ok {
			code, msg, _ := strings.Cut(status, ":")
			detail = `{"statusCode":` + code + `,"statusMsg":"` + cmp.Or(msg, "status_unavailable") + `"}`
		}
		fmt.Fprintf(w, `<html><script id="__UNIVERSAL_DATA_FOR_REHYDRATION__" type="application/json">{"__DEFAULT_SCOPE__":{"webapp.video-detail":%s}}</script></html>`, detail)
	})
	mux.HandleFunc("/media/{name}", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("tt_chain_token")
		if err != nil || !strings.HasPrefix(c.Value, "session-") || r.Referer() == "" {
			http.Error(w, "no session", http.StatusForbidden)
			return
		}
		w.Write([]byte("media:" + r.PathValue("name")))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTikTok) url(path string) string { return f.srv.URL + path }

func (f *fakeTikTok) writeChallenge(w http.ResponseWriter) {
	digest := sha256.Sum256([]byte(fakeChallengePrefix + strconv.Itoa(fakeChallengeAnswer)))
	data, _ := json.Marshal(map[string]any{"v": map[string]any{
		"a": base64.StdEncoding.EncodeToString([]byte(fakeChallengePrefix)),
		"c": base64.StdEncoding.EncodeToString(digest[:]),
	}})
	// TikTok's class attribute drops the base64 padding
	cs := strings.TrimRight(base64.StdEncoding.EncodeToString(data), "=")
	fmt.Fprintf(w, `<html><body>Please wait...<p id="cs" class="%s"></p><p id="wci" class="_wafchallengeid"></p></body></html>`, cs)
}

func (f *fakeTikTok) validAnswer(cookie string) bool {
	raw, err := base64.StdEncoding.DecodeString(cookie)
	if err != nil {
		return false
	}
	var solved struct {
		D string `json:"d"`
	}
	if json.Unmarshal(raw, &solved) != nil {
		return false
	}
	answer, _ := base64.StdEncoding.DecodeString(solved.D)
	return string(answer) == strconv.Itoa(fakeChallengeAnswer)
}

func (f *fakeTikTok) extractor(t *testing.T) *TikTokExtractor {
	t.Helper()
	config, _ := url.Parse("tiktok://" + strings.TrimPrefix(f.srv.URL, "http://") + "?insecure=1")
	ex, err := NewTikTok(config)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// readFiles reads and removes the file:// urls an extraction returned.
func readFiles(t *testing.T, urls []string) []string {
	t.Helper()
	var bodies []string
	for _, u := range urls {
		path, ok := strings.CutPrefix(u, "file://")
		if !ok || !isTempMediaFile(path) {
			t.Fatalf("want a temp media file url, got %s", u)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(path)
		bodies = append(bodies, string(b))
	}
	return bodies
}

func TestTikTokExtract(t *testing.T) {
	var f *fakeTikTok
	f = newFakeTikTok(t, nil)
	f.items = map[string]string{
		// h265 1080, h264 1080, two h264 ≤720: the best ≤720 h264 wins, first mirror 404s
		"1": `{"id":"1","desc":"a video","createTime":1704704745,"author":{"uniqueId":"someone","nickname":"Some One"},
			"stats":{"diggCount":28,"shareCount":2,"commentCount":1,"playCount":4917,"collectCount":"9"},
			"video":{"playAddr":"` + f.url("/media/play") + `","bitrateInfo":[
				{"Bitrate":336228,"CodecType":"h265_hvc1","PlayAddr":{"Width":1080,"Height":1920,"UrlList":["` + f.url("/media/h265") + `"]}},
				{"Bitrate":900000,"CodecType":"h264","PlayAddr":{"Width":1080,"Height":1920,"UrlList":["` + f.url("/media/h264-1080") + `"]}},
				{"Bitrate":156129,"CodecType":"h264","PlayAddr":{"Width":576,"Height":1024,"UrlList":["` + f.url("/media/h264-low") + `"]}},
				{"Bitrate":303209,"CodecType":"h264","PlayAddr":{"Width":576,"Height":1024,"UrlList":["` + f.url("/missing/x") + `","` + f.url("/media/h264-576") + `"]}}]}}`,
		// slideshow: jpeg mirrors picked over webp
		"2": `{"id":"2","desc":"photos","author":{"uniqueId":"p"},"video":{},"imagePost":{"images":[
			{"imageURL":{"urlList":["` + f.url("/media/a.webp?x") + `","` + f.url("/media/a.jpeg?x") + `"]}},
			{"imageURL":{"urlList":["` + f.url("/media/b.jpeg?x") + `"]}}]}}`,
		// no h264 rendition: the default playAddr
		"3": `{"id":"3","video":{"playAddr":"` + f.url("/media/play") + `","bitrateInfo":[{"Bitrate":1,"CodecType":"bytevc2","PlayAddr":{"UrlList":["` + f.url("/media/vvc") + `"]}}]}}`,
		"4": "status:10231",
		"5": `{"id":"5","isContentClassified":true,"video":{}}`,
		"6": `{"id":"6","video":{"playAddr":"` + f.url("/missing/y") + `"}}`,
		"7": "status:10204:status_deleted",
		"8": "status:10204:item doesn't exist",
		"9": "status:10204:status_self_see",
	}
	ex := f.extractor(t)

	tests := []struct {
		id      string
		want    []string
		wantErr string
		noVideo bool
	}{
		{id: "1", want: []string{"media:h264-576"}},
		{id: "2", want: []string{"media:a.jpeg", "media:b.jpeg"}},
		{id: "3", want: []string{"media:play"}},
		{id: "4", wantErr: "status 10231"},
		{id: "5", wantErr: "age-restricted"},
		{id: "6", wantErr: "downloading tiktok media"},
		{id: "7", noVideo: true},
		{id: "8", noVideo: true},
		{id: "9", wantErr: "status_self_see"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			media, err := ex.ExtractMedia(context.Background(), "https://www.tiktok.com/@someone/video/"+tt.id)
			if tt.noVideo {
				if !errors.Is(err, ErrNoVideo) {
					t.Fatalf("want ErrNoVideo, got %v", err)
				}
				return
			}
			if tt.wantErr != "" {
				// all plain errors, so the next extractor still tries
				if err == nil || errors.Is(err, ErrNoVideo) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want a plain error with %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readFiles(t, media.URLs); fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("media = %v, want %v", got, tt.want)
			}
		})
	}

	media, err := ex.ExtractMedia(context.Background(), "https://www.tiktok.com/@someone/video/1")
	if err != nil {
		t.Fatal(err)
	}
	readFiles(t, media.URLs)
	m := media.Metadata
	if m.Description != "a video" || m.Author != "Some One" || m.AuthorHandle != "someone" || m.Likes != 28 || m.Views != 4917 ||
		m.Comments != 1 || m.Shares != 2 || m.Source != "tiktok" || !m.PostedAt.Equal(time.Unix(1704704745, 0)) {
		t.Errorf("metadata = %+v", m)
	}
}

func TestTikTokChallenge(t *testing.T) {
	f := newFakeTikTok(t, nil)
	f.items = map[string]string{"1": `{"id":"1","video":{"playAddr":"` + f.url("/media/play") + `"}}`}
	f.challenge.Store(true)

	media, err := f.extractor(t).ExtractMedia(context.Background(), "https://www.tiktok.com/@someone/video/1")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFiles(t, media.URLs); fmt.Sprint(got) != "[media:play]" {
		t.Fatalf("media = %v", got)
	}
}

// TestTikTokLive extracts real posts through the proxy env:
//
//	TIKTOK_LIVE=1 TIKTOK_LIVE_URLS=https://www.tiktok.com/@user/video/123,... go test ./preview -run TestTikTokLive -v
func TestTikTokLive(t *testing.T) {
	if os.Getenv("TIKTOK_LIVE") == "" {
		t.Skip("set TIKTOK_LIVE=1 to call TikTok")
	}
	ex, _ := NewTikTok(&url.URL{Scheme: "tiktok"})
	for _, u := range strings.Split(os.Getenv("TIKTOK_LIVE_URLS"), ",") {
		start := time.Now()
		media, err := ex.ExtractMedia(context.Background(), u)
		if err != nil {
			t.Logf("FAIL %s (%s): %v", u, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		var sizes []string
		for _, f := range media.URLs {
			path := strings.TrimPrefix(f, "file://")
			b, _ := os.ReadFile(path)
			sizes = append(sizes, fmt.Sprintf("%s %dKB", detectContentType(b), len(b)/1024))
			os.Remove(path)
		}
		t.Logf("OK %s (%s): %v %q by @%s", u, time.Since(start).Round(time.Millisecond), sizes, truncate(media.Metadata.Description, 40), media.Metadata.AuthorHandle)
	}
}
