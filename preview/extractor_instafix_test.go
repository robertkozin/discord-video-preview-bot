package preview

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func newTestInstaFix(t *testing.T, handler http.HandlerFunc, options string) (*InstaFixExtractor, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	config, err := url.Parse("instafix://" + strings.TrimPrefix(srv.URL, "http://") + "?insecure=1" + options)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := NewInstaFix(config)
	if err != nil {
		t.Fatal(err)
	}
	return ex, srv
}

// instaFixServer answers like an InstaFix fork: relative media urls, items
// numbered from 1 under /p/<id>/<n>, an empty page past the last item.
func instaFixServer(items []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != instaFixUserAgent {
			http.Error(w, "want the Discordbot user agent", http.StatusTeapot)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/p/ABC123/%d", &n); err != nil {
			n = 1
		}
		fmt.Fprint(w, `<!doctype html><html><head><meta name="twitter:title" content="@some.user"/>`)
		if n >= 1 && n <= len(items) {
			prop := "og:image"
			if strings.HasPrefix(items[n-1], "/videos/") {
				prop = "og:video"
			}
			fmt.Fprintf(w, `<meta property="%s" content="%s"/>`, prop, items[n-1])
		}
		fmt.Fprint(w, `</head><body><meta property="og:video" content="/ignored-in-body"/></body></html>`)
	}
}

func TestInstaFixCarousel(t *testing.T) {
	ex, srv := newTestInstaFix(t, instaFixServer([]string{"/images/ABC123/1", "/videos/ABC123/2?audio=1", "/images/ABC123/3"}), "")

	media, err := ex.ExtractMedia(context.Background(), "https://www.instagram.com/p/ABC123/?igsh=xyz")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{srv.URL + "/images/ABC123/1", srv.URL + "/videos/ABC123/2?audio=1", srv.URL + "/images/ABC123/3"}
	if fmt.Sprint(media.URLs) != fmt.Sprint(want) {
		t.Errorf("urls = %v, want %v", media.URLs, want)
	}
	if media.Metadata == nil || media.Metadata.AuthorHandle != "some.user" {
		t.Errorf("metadata = %+v, want handle some.user", media.Metadata)
	}
	if name := extractorName(ex); !strings.HasPrefix(name, "instafix:127.0.0.1:") {
		t.Errorf("extractorName = %q", name)
	}
}

// A reel is never a carousel, so only its own page is fetched.
func TestInstaFixReelFetchesOnePage(t *testing.T) {
	var paths []string
	ex, srv := newTestInstaFix(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		fmt.Fprint(w, `<html><head><meta property="og:video" content="/videos/XYZ/1"></head></html>`)
	}, "")

	urls, err := ex.Extract(context.Background(), "https://instagram.com/reels/XYZ/")
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != srv.URL+"/videos/XYZ/1" {
		t.Errorf("urls = %v", urls)
	}
	if fmt.Sprint(paths) != "[/reel/XYZ/]" {
		t.Errorf("fetched %v, want just /reel/XYZ/", paths)
	}
}

// With carousel=0 (vxinstagram, which answers past the end of a carousel) only
// the post's own page is used, and its "Name (@handle)" title and caption are
// read.
func TestInstaFixNoCarousel(t *testing.T) {
	var paths []string
	ex, _ := newTestInstaFix(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		fmt.Fprint(w, `<html><head>
			<meta property="og:title" content="The Paper (@the.paper)" />
			<meta property="og:description" content="Senator says &#x27;many&#x27; things" />
			<meta property="og:image" content="https://cdn.example/offload/ABC123/0" />
		</head></html>`)
	}, "&carousel=0")

	media, err := ex.ExtractMedia(context.Background(), "https://www.instagram.com/some.user/p/ABC123/")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(media.URLs) != "[https://cdn.example/offload/ABC123/0]" || fmt.Sprint(paths) != "[/p/ABC123/]" {
		t.Errorf("urls = %v, fetched %v", media.URLs, paths)
	}
	m := media.Metadata
	if m == nil || m.Author != "The Paper" || m.AuthorHandle != "the.paper" || m.Description != "Senator says 'many' things" {
		t.Errorf("metadata = %+v", m)
	}
}

// A fixer that can't get the post redirects to instagram.com or answers with
// an error or an empty page; all of them are failures, so the next extractor
// gets its turn.
func TestInstaFixFailures(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://www.instagram.com"+r.URL.Path, http.StatusFound)
		},
		"404": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"no meta": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `<html><head><title>InstaFix</title></head></html>`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ex, _ := newTestInstaFix(t, handler, "")
			if _, err := ex.Extract(context.Background(), "https://www.instagram.com/p/ABC123/"); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestInstagramPost(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.instagram.com/p/DfapgI6kxuF/?srtk=x": "p DfapgI6kxuF",
		"https://instagram.com/reel/Ddt7brNAtYY":          "reel Ddt7brNAtYY",
		"https://www.instagram.com/reels/Ddt7brNAtYY/":    "reel Ddt7brNAtYY",
		"https://www.instagram.com/tv/Ddt7brNAtYY/":       "p Ddt7brNAtYY",
		"https://www.instagram.com/some.user/p/Abc_-1/":   "p Abc_-1",
		"https://www.instagram.com/some.user/":            "error",
		"https://www.instagram.com/stories/someone/123/":  "error",
		"https://www.instagram.com/p/":                    "error",
		"https://example.com/p/ABC/":                      "error",
	} {
		kind, id, err := instagramPost(in)
		got := kind + " " + id
		if err != nil {
			got = "error"
		}
		if got != want {
			t.Errorf("instagramPost(%q) = %q, want %q", in, got, want)
		}
	}
}
