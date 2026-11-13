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
)

// xResponses are trimmed copies of real tweet-result responses, keyed by id.
var xResponses = map[string]string{
	// video with an m3u8 and four mp4s; the text ends with the media's t.co link
	"1": `{"__typename":"Tweet","text":"Protesters &amp; police 🏴󠁧󠁢󠁥󠁮󠁧󠁿 outside https://t.co/AAA https://t.co/MED",
		"display_text_range":[0,63],"created_at":"2026-11-11T16:45:09.000Z","favorite_count":7608,"conversation_count":357,
		"entities":{"urls":[{"url":"https://t.co/AAA","expanded_url":"https://example.com/story"}],"media":[{"url":"https://t.co/MED"}]},
		"user":{"name":"Clash Report","screen_name":"clashreport"},
		"mediaDetails":[{"type":"video","media_url_https":"https://pbs.twimg.com/amplify_video_thumb/1/img/a.jpg","video_info":{"variants":[
			{"content_type":"application/x-mpegURL","url":"https://video.twimg.com/amplify_video/1/pl/a.m3u8"},
			{"bitrate":632000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/1/vid/avc1/320x568/a.mp4"},
			{"bitrate":2176000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/1/vid/avc1/720x1280/b.mp4"},
			{"bitrate":10368000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/1/vid/avc1/1080x1920/c.mp4"},
			{"bitrate":950000,"content_type":"video/mp4","url":"https://video.twimg.com/amplify_video/1/vid/avc1/480x852/d.mp4"}]}}]}`,
	// two photos
	"2": `{"__typename":"Tweet","text":"two pics https://t.co/MED","display_text_range":[0,8],"user":{"name":"A","screen_name":"a"},
		"entities":{"media":[{"url":"https://t.co/MED"}]},
		"mediaDetails":[{"type":"photo","media_url_https":"https://pbs.twimg.com/media/A.png"},{"type":"photo","media_url_https":"https://pbs.twimg.com/media/B.jpg"}]}`,
	// text only
	"3": `{"__typename":"Tweet","text":"just words","display_text_range":[0,10],"user":{"name":"A","screen_name":"a"}}`,
	// quoting a video, no media of its own
	"4": `{"__typename":"Tweet","text":"look at this","display_text_range":[0,12],"user":{"name":"A","screen_name":"a"},
		"quoted_tweet":{"__typename":"Tweet","text":"q","mediaDetails":[{"type":"animated_gif","video_info":{"variants":[
			{"bitrate":0,"content_type":"video/mp4","url":"https://video.twimg.com/tweet_video/G.mp4"}]}}]}}`,
	// needs a login
	"5": `{"__typename":"TweetTombstone","tombstone":{}}`,
	// a link card, no media
	"6": `{"__typename":"Tweet","text":"read https://t.co/AAA","display_text_range":[0,21],"user":{"name":"A","screen_name":"a"},"card":{"name":"summary_large_image"}}`,
	// long post: text cut short
	"7": `{"__typename":"Tweet","text":"first line\nsecond","display_text_range":[0,17],"note_tweet":{"id":"x"},"user":{"name":"A","screen_name":"a"},
		"mediaDetails":[{"type":"photo","media_url_https":"https://pbs.twimg.com/media/C.jpg"}]}`,
	// only 1080p
	"8": `{"__typename":"Tweet","text":"hd","display_text_range":[0,2],"user":{"name":"A","screen_name":"a"},
		"mediaDetails":[{"type":"video","video_info":{"variants":[
			{"bitrate":10368000,"content_type":"video/mp4","url":"https://video.twimg.com/v/vid/avc1/1080x1920/big.mp4"},
			{"bitrate":5000000,"content_type":"video/mp4","url":"https://video.twimg.com/v/vid/avc1/1080x1080/smaller.mp4"}]}}]}`,
}

func newTestX(t *testing.T) *XExtractor {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tweet-result" || r.URL.Query().Get("token") == "" {
			w.Write([]byte(`{}`))
			return
		}
		body, ok := xResponses[r.URL.Query().Get("id")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	config, _ := url.Parse("x://" + strings.TrimPrefix(srv.URL, "http://") + "?insecure=1")
	ex, err := NewX(config)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func TestXExtract(t *testing.T) {
	ex := newTestX(t)
	tests := []struct {
		id      string
		want    []string
		noVideo bool
		wantErr bool
	}{
		{id: "1", want: []string{"https://video.twimg.com/amplify_video/1/vid/avc1/720x1280/b.mp4"}},
		{id: "2", want: []string{"https://pbs.twimg.com/media/A.png?name=orig", "https://pbs.twimg.com/media/B.jpg?name=orig"}},
		{id: "3", noVideo: true},
		{id: "4", want: []string{"https://video.twimg.com/tweet_video/G.mp4"}},
		{id: "5", wantErr: true},
		{id: "6", wantErr: true},
		{id: "8", want: []string{"https://video.twimg.com/v/vid/avc1/1080x1080/smaller.mp4"}},
		{id: "404", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			media, err := ex.ExtractMedia(context.Background(), "https://x.com/someone/status/"+tt.id+"?s=46")
			switch {
			case tt.noVideo:
				if !errors.Is(err, ErrNoVideo) {
					t.Fatalf("want ErrNoVideo, got %v", err)
				}
			case tt.wantErr:
				// must be a plain error, so the next extractor (fxtwitter) still tries
				if err == nil || errors.Is(err, ErrNoVideo) {
					t.Fatalf("want a plain error, got %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(media.URLs) != fmt.Sprint(tt.want) {
					t.Fatalf("urls = %v, want %v", media.URLs, tt.want)
				}
			}
		})
	}
}

func TestXMetadata(t *testing.T) {
	ex := newTestX(t)
	media, err := ex.ExtractMedia(context.Background(), "https://twitter.com/clashreport/status/1")
	if err != nil {
		t.Fatal(err)
	}
	m := media.Metadata
	// display range is in UTF-16 units (the flag is 14 of them), media link dropped, t.co expanded, &amp; decoded
	if want := "Protesters & police 🏴󠁧󠁢󠁥󠁮󠁧󠁿 outside https://example.com/story"; m.Description != want {
		t.Errorf("description = %q, want %q", m.Description, want)
	}
	if m.Author != "Clash Report" || m.AuthorHandle != "clashreport" || m.Likes != 7608 || m.Comments != 357 || m.Source != "x" {
		t.Errorf("metadata = %+v", m)
	}
	if m.PostedAt.IsZero() {
		t.Error("posted_at not parsed")
	}

	media, err = ex.ExtractMedia(context.Background(), "https://x.com/a/status/7")
	if err != nil {
		t.Fatal(err)
	}
	if want := "first line\nsecond…"; media.Metadata.Description != want {
		t.Errorf("long post description = %q, want %q", media.Metadata.Description, want)
	}
}

// TestXLive checks real posts against the real API, through the proxy env:
//
//	X_LIVE=1 X_LIVE_IDS=2120275724657393985,2120558459498139868 go test ./preview -run TestXLive -v
func TestXLive(t *testing.T) {
	if os.Getenv("X_LIVE") == "" {
		t.Skip("set X_LIVE=1 to call the real API")
	}
	ex, _ := NewX(&url.URL{Scheme: "x"})
	for _, id := range strings.Split(os.Getenv("X_LIVE_IDS"), ",") {
		media, err := ex.ExtractMedia(context.Background(), "https://x.com/i/status/"+id)
		if err != nil {
			t.Logf("%s: %v", id, err)
			continue
		}
		t.Logf("%s: %v %q by @%s", id, media.URLs, truncate(media.Metadata.Description, 60), media.Metadata.AuthorHandle)
	}
}
