package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// embedPage wraps a post like Instagram's embed page: contextJSON is a JSON string inside a script's JSON.
func embedPage(contextJSON string) string {
	b, _ := json.Marshal(contextJSON)
	return `<html><script type="application/json">{"props":{"isProfileEmbed":false,"contextJSON":` + string(b) + `,"x":1}}</script></html>`
}

func TestInstagramEmbed(t *testing.T) {
	pages := map[string]string{
		"Dfp6E98Bjxr": embedPage(`{"context":{"type":"GraphVideo"},"gql_data":{"shortcode_media":{"__typename":"GraphVideo","is_video":true,
			"video_url":"https://cdn/v.mp4?a=1&b=2","display_url":"https://cdn/thumb.jpg","video_view_count":77,"taken_at_timestamp":1794515272,
			"owner":{"username":"someone","full_name":"Some One"},"edge_media_to_caption":{"edges":[{"node":{"text":" hi \"there\" "}}]}}}}`),
		"DfpiweAilxF": embedPage(`{"gql_data":{"shortcode_media":{"__typename":"GraphSidecar","owner":{"username":"c"},"edge_sidecar_to_children":{"edges":[
			{"node":{"display_url":"https://cdn/a.jpg"}},{"node":{"is_video":true,"video_url":"https://cdn/b.mp4","display_url":"https://cdn/b.jpg"}}]}}}}`),
		"DfnPQv7hWbz": `<html>WatchOnInstagram</html>`,
		"AAAAAAAAAAA": `<html>error</html>`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "curl/") {
			fmt.Fprint(w, `<html>error page</html>`)
			return
		}
		code := strings.Split(r.URL.Path, "/")[2]
		fmt.Fprint(w, pages[code])
	}))
	defer srv.Close()
	ex, _ := NewInstagramEmbed(&url.URL{Scheme: "instagramembed", Host: strings.TrimPrefix(srv.URL, "http://"), RawQuery: "insecure=1"})

	media, err := ex.ExtractMedia(context.Background(), "https://www.instagram.com/reel/Dfp6E98Bjxr/")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(media.URLs) != "[https://cdn/v.mp4?a=1&b=2]" {
		t.Errorf("urls = %v", media.URLs)
	}
	m := media.Metadata
	if m.Description != `hi "there"` || m.AuthorHandle != "someone" || m.Author != "Some One" || m.Views != 77 || m.Source != "instagram" || !m.PostedAt.Equal(time.Unix(1794515272, 0)) {
		t.Errorf("metadata = %+v", m)
	}

	media, err = ex.ExtractMedia(context.Background(), "https://www.instagram.com/p/DfpiweAilxF/")
	if err != nil || fmt.Sprint(media.URLs) != "[https://cdn/a.jpg https://cdn/b.mp4]" {
		t.Errorf("carousel = %v, %v", media.URLs, err)
	}

	for _, code := range []string{"DfnPQv7hWbz", "AAAAAAAAAAA"} {
		if _, err := ex.ExtractMedia(context.Background(), "https://www.instagram.com/p/"+code+"/"); err == nil {
			t.Errorf("%s: want an error", code)
		} else if strings.Contains(err.Error(), "no video") {
			t.Errorf("%s: should be a plain error: %v", code, err)
		}
	}
}

// TestInstagramEmbedLive: INSTAGRAM_EMBED_LIVE=1 INSTAGRAM_LIVE_URLS=https://www.instagram.com/reel/…,…
func TestInstagramEmbedLive(t *testing.T) {
	if os.Getenv("INSTAGRAM_EMBED_LIVE") == "" {
		t.Skip("set INSTAGRAM_EMBED_LIVE=1 to call Instagram")
	}
	ex, _ := NewInstagramEmbed(&url.URL{Scheme: "instagramembed"})
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
		t.Logf("OK %s (%s): %v %q by @%s views=%d", u, time.Since(start).Round(time.Millisecond), sizes, truncate(media.Metadata.Description, 40), media.Metadata.AuthorHandle, media.Metadata.Views)
	}
}
