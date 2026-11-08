package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const redditTestManifest = `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period>
	<AdaptationSet contentType="video">
		<Representation bandwidth="400000" codecs="avc1.4d401e" height="480" width="270"><BaseURL>CMAF_270.mp4</BaseURL></Representation>
		<Representation bandwidth="1100000" codecs="avc1.4d401f" height="1280" width="720"><BaseURL>CMAF_720.mp4</BaseURL></Representation>
		<Representation bandwidth="5000000" codecs="avc1.640028" height="1920" width="1080"><BaseURL>CMAF_1080.mp4</BaseURL></Representation>
		<Representation bandwidth="900000" codecs="hvc1.1.6" height="1280" width="720"><BaseURL>HEVC_720.mp4</BaseURL></Representation>
	</AdaptationSet>
	<AdaptationSet contentType="audio">
		<Representation bandwidth="67000" codecs="mp4a.40.2"><BaseURL>CMAF_AUDIO_64.mp4</BaseURL></Representation>
		<Representation bandwidth="130000" codecs="mp4a.40.2"><BaseURL>CMAF_AUDIO_128.mp4</BaseURL></Representation>
	</AdaptationSet>
</Period></MPD>`

const redditTestSilentManifest = `<MPD><Period><AdaptationSet>
	<Representation mimeType="video/mp4" bandwidth="400000" height="480" width="270"><BaseURL>DASH_270.mp4</BaseURL></Representation>
</AdaptationSet></Period></MPD>`

func TestPickRedditTracks(t *testing.T) {
	base, _ := url.Parse("https://v.redd.it/abc/DASHPlaylist.mpd?a=signed&v=1")

	video, audio, err := pickRedditTracks([]byte(redditTestManifest), base)
	if err != nil {
		t.Fatal(err)
	}
	if video != "https://v.redd.it/abc/CMAF_720.mp4" || audio != "https://v.redd.it/abc/CMAF_AUDIO_128.mp4" {
		t.Errorf("video = %q, audio = %q", video, audio)
	}

	// older manifests only set a mime type, and silent videos have no audio set
	video, audio, err = pickRedditTracks([]byte(redditTestSilentManifest), base)
	if err != nil {
		t.Fatal(err)
	}
	if video != "https://v.redd.it/abc/DASH_270.mp4" || audio != "" {
		t.Errorf("silent: video = %q, audio = %q", video, audio)
	}
}

func TestPickRedditTracksTooBigTakesSmallest(t *testing.T) {
	base, _ := url.Parse("https://v.redd.it/abc/DASHPlaylist.mpd")
	manifest := `<MPD><Period><AdaptationSet contentType="video">
		<Representation bandwidth="9000000" height="2160" width="3840"><BaseURL>big.mp4</BaseURL></Representation>
		<Representation bandwidth="5000000" height="1080" width="1920"><BaseURL>less.mp4</BaseURL></Representation>
	</AdaptationSet></Period></MPD>`
	video, _, err := pickRedditTracks([]byte(manifest), base)
	if err != nil || video != "https://v.redd.it/abc/less.mp4" {
		t.Errorf("video = %q, err = %v", video, err)
	}
}

func TestRedditLookup(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.reddit.com/r/cats/comments/1xqguhr/neighbor_cat/":    "id=t3_1xqguhr",
		"https://old.reddit.com/r/cats/comments/1xqguhr/":                 "id=t3_1xqguhr",
		"https://www.reddit.com/user/someone/comments/1xqguhr/a_title/":   "id=t3_1xqguhr",
		"https://www.reddit.com/r/cats/comments/1xqguhr/a_title/o1abcde/": "id=t3_1xqguhr",
		"https://redd.it/1xqguhr":                                         "id=t3_1xqguhr",
		"https://v.redd.it/j9ymzohqk90i1":                                 "url=https%3A%2F%2Fv.redd.it%2Fj9ymzohqk90i1",
		"https://v.redd.it/j9ymzohqk90i1/DASH_720.mp4":                    "url=https%3A%2F%2Fv.redd.it%2Fj9ymzohqk90i1",
		"https://www.reddit.com/r/cats/":                                  "error",
		"https://www.reddit.com/r/cats/comments/":                         "error",
		"https://example.com/r/cats/comments/1xqguhr/":                    "error",
		"https://v.redd.it/":                                              "error",
	} {
		got := "error"
		if q, err := redditLookup(in); err == nil {
			got = q.Encode()
		}
		if got != want {
			t.Errorf("redditLookup(%q) = %q, want %q", in, got, want)
		}
	}
}

type redditTestServer struct {
	*httptest.Server
	post     map[string]any
	manifest string
	lookups  []string
}

func newRedditTest(t *testing.T, post map[string]any, manifest string) (*RedditExtractor, *redditTestServer, *[][]string) {
	t.Helper()
	ts := &redditTestServer{post: post, manifest: manifest}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if id, secret, _ := r.BasicAuth(); id != "id" || secret != "secret" {
			http.Error(w, "bad client", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "bearer tok" || r.UserAgent() != RedditUserAgent {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ts.lookups = append(ts.lookups, r.URL.RawQuery)
		children := []any{}
		if ts.post != nil {
			children = append(children, map[string]any{"data": ts.post})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"children": children}})
	})
	mux.HandleFunc("/abc/DASHPlaylist.mpd", func(w http.ResponseWriter, r *http.Request) {
		if ts.manifest == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, ts.manifest)
	})
	ts.Server = httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	muxed := &[][]string{}
	ex := &RedditExtractor{
		Endpoint: ts.URL,
		auth:     &RedditAuth{TokenURL: ts.URL + "/token", clientID: "id", clientSecret: "secret", refreshToken: "refresh"},
		mux: func(_ context.Context, inputs ...string) (string, error) {
			*muxed = append(*muxed, inputs)
			return "file:///tmp/merged.mp4", nil
		},
	}
	return ex, ts, muxed
}

func redditTestPost(srv string, hasAudio bool) map[string]any {
	return map[string]any{
		"title": "Japanese Halloween", "author": "someone", "score": 32386, "num_comments": 234, "created_utc": 1794089384.0,
		"secure_media": map[string]any{"reddit_video": map[string]any{
			"fallback_url": "https://v.redd.it/abc/CMAF_480.mp4?source=fallback",
			"dash_url":     "DASH",
			"has_audio":    hasAudio,
		}},
	}
}

func setDash(post map[string]any, url string) {
	post["secure_media"].(map[string]any)["reddit_video"].(map[string]any)["dash_url"] = url
}

const redditTestURL = "https://www.reddit.com/r/cats/comments/1xqguhr/a_title/"

func TestRedditMergesVideoAndAudio(t *testing.T) {
	ex, ts, muxed := newRedditTest(t, nil, redditTestManifest)
	ts.post = redditTestPost(ts.URL, true)
	setDash(ts.post, ts.URL+"/abc/DASHPlaylist.mpd?a=signed")

	media, err := ex.ExtractMedia(context.Background(), redditTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(media.URLs) != "[file:///tmp/merged.mp4]" {
		t.Errorf("urls = %v", media.URLs)
	}
	want := fmt.Sprintf("[[%s/abc/CMAF_720.mp4 %s/abc/CMAF_AUDIO_128.mp4]]", ts.URL, ts.URL)
	if fmt.Sprint(*muxed) != want {
		t.Errorf("muxed %v, want %v", *muxed, want)
	}
	if fmt.Sprint(ts.lookups) != "[id=t3_1xqguhr&raw_json=1]" {
		t.Errorf("lookups = %v", ts.lookups)
	}
	m := media.Metadata
	if m == nil || m.Title != "Japanese Halloween" || m.AuthorHandle != "someone" || m.Likes != 32386 || m.Comments != 234 || m.PostedAt.IsZero() {
		t.Errorf("metadata = %+v", m)
	}
}

// A silent video needs no ffmpeg, and can use the fallback url without a manifest.
func TestRedditSilentVideo(t *testing.T) {
	ex, ts, muxed := newRedditTest(t, nil, redditTestSilentManifest)
	ts.post = redditTestPost(ts.URL, false)
	setDash(ts.post, ts.URL+"/abc/DASHPlaylist.mpd")

	urls, err := ex.Extract(context.Background(), redditTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(urls) != "["+ts.URL+"/abc/DASH_270.mp4]" || len(*muxed) != 0 {
		t.Errorf("urls = %v, muxed = %v", urls, *muxed)
	}

	ts.manifest = ""
	urls, err = ex.Extract(context.Background(), redditTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(urls) != "[https://v.redd.it/abc/CMAF_480.mp4?source=fallback]" {
		t.Errorf("without a manifest: urls = %v", urls)
	}
}

// The fallback url has no sound, so it must not pass for a video that has some.
func TestRedditNeverReturnsMuteVideoWithSound(t *testing.T) {
	for name, manifest := range map[string]string{
		"manifest missing": "",
		"no audio track":   redditTestSilentManifest,
	} {
		t.Run(name, func(t *testing.T) {
			ex, ts, _ := newRedditTest(t, nil, manifest)
			ts.post = redditTestPost(ts.URL, true)
			setDash(ts.post, ts.URL+"/abc/DASHPlaylist.mpd")
			if urls, err := ex.Extract(context.Background(), redditTestURL); err == nil {
				t.Errorf("want an error, got %v", urls)
			}
		})
	}
}

func TestRedditCrosspostUsesParentVideo(t *testing.T) {
	ex, ts, _ := newRedditTest(t, nil, redditTestSilentManifest)
	parent := redditTestPost(ts.URL, false)
	setDash(parent, ts.URL+"/abc/DASHPlaylist.mpd")
	ts.post = map[string]any{"title": "Crosspost", "author": "other", "crosspost_parent_list": []any{parent}}

	media, err := ex.ExtractMedia(context.Background(), redditTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(media.URLs[0], "/abc/DASH_270.mp4") || media.Metadata.Title != "Crosspost" {
		t.Errorf("urls = %v, metadata = %+v", media.URLs, media.Metadata)
	}
}

// These aren't ErrNoVideo: other extractors may still get something.
func TestRedditFailuresLeaveOthersToTry(t *testing.T) {
	for name, post := range map[string]map[string]any{
		"not found":    nil,
		"text or link": {"title": "just text", "author": "someone"},
	} {
		t.Run(name, func(t *testing.T) {
			ex, _, _ := newRedditTest(t, post, "")
			_, err := ex.Extract(context.Background(), redditTestURL)
			if err == nil || strings.Contains(err.Error(), ErrNoVideo.Error()) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestRedditDeletedAuthorIsDropped(t *testing.T) {
	ex, ts, _ := newRedditTest(t, map[string]any{"title": "t", "author": "[deleted]"}, "")
	_ = ts
	m, err := ex.Metadata(context.Background(), redditTestURL)
	if err != nil {
		t.Fatal(err)
	}
	if m.AuthorHandle != "" {
		t.Errorf("handle = %q", m.AuthorHandle)
	}
}
