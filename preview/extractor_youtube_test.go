package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestYouTubeShortID(t *testing.T) {
	tests := map[string]string{
		"https://www.youtube.com/shorts/UeCMLVAZMc8":    "UeCMLVAZMc8",
		"https://youtube.com/shorts/UeCMLVAZMc8?si=abc": "UeCMLVAZMc8",
		"https://m.youtube.com/shorts/UeCMLVAZMc8/":     "UeCMLVAZMc8",
		"https://www.youtube.com/watch?v=UeCMLVAZMc8":   "",
		"https://youtu.be/UeCMLVAZMc8":                  "",
		"https://www.youtube.com/shorts/short":          "",
		"https://www.notyoutube.com/shorts/UeCMLVAZMc8": "",
		"https://www.youtube.com/@channel/shorts":       "",
	}
	for in, want := range tests {
		if got, _ := youtubeShortID(in); got != want {
			t.Errorf("youtubeShortID(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeYouTube answers sw.js_data, the player API (bot check without a known
// visitor id) and ranged media requests (a request without a range fails, so
// the throttled path can't pass by accident).
type fakeYouTube struct {
	srv          *httptest.Server
	players      map[string]string // video id → player JSON, with {{media}} for the server url
	visitor      string            // the id sw.js_data hands out
	acceptedVis  atomic.Value      // string: the visitor id the player accepts
	visitorCalls atomic.Int32
	rangeCalls   atomic.Int32
}

func newFakeYouTube(t *testing.T) *fakeYouTube {
	t.Helper()
	f := &fakeYouTube{players: map[string]string{}, visitor: "CgtWSVNJVE9SXzE%3D"}
	f.acceptedVis.Store("CgtWSVNJVE9SXzE=")
	mux := http.NewServeMux()
	mux.HandleFunc("/sw.js_data", func(w http.ResponseWriter, r *http.Request) {
		f.visitorCalls.Add(1)
		fmt.Fprintf(w, `)]}'`+"\n"+`[["yt.sw.adr",null,[[["en","CA",null,"1.2.3.4",null,null,null,null,null,null,null,null,null,"%s"]]]]]`, f.visitor)
	})
	mux.HandleFunc("POST /youtubei/v1/player", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Context struct {
				Client struct {
					ClientName  string `json:"clientName"`
					VisitorData string `json:"visitorData"`
				} `json:"client"`
			} `json:"context"`
			VideoID string `json:"videoId"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Context.Client.ClientName != "VISIONOS" || r.Header.Get("X-YouTube-Client-Name") != "101" {
			http.Error(w, `{"error":{"code":400,"message":"bad client"}}`, http.StatusBadRequest)
			return
		}
		if body.Context.Client.VisitorData != f.acceptedVis.Load().(string) || r.Header.Get("X-Goog-Visitor-Id") != body.Context.Client.VisitorData {
			fmt.Fprint(w, `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm you’re not a bot"}}`)
			return
		}
		player, ok := f.players[body.VideoID]
		if !ok {
			player = `{"playabilityStatus":{"status":"ERROR","reason":"This video is unavailable"}}`
		}
		fmt.Fprint(w, strings.ReplaceAll(player, "{{media}}", f.srv.URL+"/media"))
	})
	mux.HandleFunc("/media/{name}", func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseRange(r.URL.Query().Get("range"))
		if !ok {
			http.Error(w, "throttled", http.StatusTooManyRequests)
			return
		}
		f.rangeCalls.Add(1)
		content := mediaContent(r.PathValue("name"))
		if end >= len(content) {
			http.Error(w, "range past the end", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.Write([]byte(content[start : end+1]))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// mediaContent is a deterministic body per media name, 25 bytes long.
func mediaContent(name string) string { return fmt.Sprintf("%-25s", "media:"+name)[:25] }

func parseRange(r string) (int, int, bool) {
	a, b, ok := strings.Cut(r, "-")
	start, err1 := strconv.Atoi(a)
	end, err2 := strconv.Atoi(b)
	return start, end, ok && err1 == nil && err2 == nil && start <= end
}

func (f *fakeYouTube) extractor(t *testing.T) *YouTubeExtractor {
	t.Helper()
	config, _ := url.Parse("youtube://" + strings.TrimPrefix(f.srv.URL, "http://") + "?insecure=1")
	ex, err := NewYouTube(config)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

func youtubeFormatJSON(itag int, mime string, w, h int, bitrate int, name string, drc bool) string {
	return fmt.Sprintf(`{"itag":%d,"url":"{{media}}/%s?itag=%d","mimeType":%q,"bitrate":%d,"width":%d,"height":%d,"contentLength":"25","isDrc":%t}`,
		itag, name, itag, mime, bitrate, w, h, drc)
}

func TestYouTubePickFormats(t *testing.T) {
	var p youtubePlayer
	adaptive := []string{
		youtubeFormatJSON(137, `video/mp4; codecs="avc1.640028"`, 1080, 1920, 4000000, "v1080", false),
		youtubeFormatJSON(136, `video/mp4; codecs="avc1.4D401F"`, 720, 1280, 1500000, "v720", false),
		youtubeFormatJSON(135, `video/mp4; codecs="avc1.4D401F"`, 480, 854, 700000, "v480", false),
		youtubeFormatJSON(398, `video/mp4; codecs="av01.0.05M.08"`, 720, 1280, 1800000, "av1", false),
		youtubeFormatJSON(247, `video/webm; codecs="vp9"`, 720, 1280, 1900000, "vp9", false),
		youtubeFormatJSON(140, `audio/mp4; codecs="mp4a.40.2"`, 0, 0, 130355, "a140drc", true),
		youtubeFormatJSON(140, `audio/mp4; codecs="mp4a.40.2"`, 0, 0, 130348, "a140", false),
		youtubeFormatJSON(139, `audio/mp4; codecs="mp4a.40.5"`, 0, 0, 49833, "a139", false),
		youtubeFormatJSON(251, `audio/webm; codecs="opus"`, 0, 0, 150000, "opus", false),
	}
	if err := json.Unmarshal([]byte(`{"streamingData":{"adaptiveFormats":[`+strings.Join(adaptive, ",")+`]}}`), &p); err != nil {
		t.Fatal(err)
	}
	video, audio := pickYouTubeFormats(&p)
	if video == nil || audio == nil || !strings.Contains(video.URL, "/v720") || !strings.Contains(audio.URL, "/a140?") {
		t.Fatalf("picked %+v and %+v", video, audio)
	}
}

func TestYouTubeExtract(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("needs ffmpeg")
	}
	f := newFakeYouTube(t)
	// real (tiny) media so ffmpeg can mux it
	video, audio := tinyMP4(t, "video"), tinyMP4(t, "audio")
	mux := http.NewServeMux()
	mux.HandleFunc("/{name}", func(w http.ResponseWriter, r *http.Request) {
		body := map[string][]byte{"v": video, "a": audio}[r.PathValue("name")]
		start, end, ok := parseRange(r.URL.Query().Get("range"))
		if !ok || end >= len(body) {
			http.Error(w, "need a range", http.StatusBadRequest)
			return
		}
		f.rangeCalls.Add(1)
		w.Write(body[start : end+1])
	})
	media := httptest.NewServer(mux)
	t.Cleanup(media.Close)
	f.players["UeCMLVAZMc8"] = `{"playabilityStatus":{"status":"OK"},
		"videoDetails":{"title":"Matrix scene","author":"Duhek clips","shortDescription":"#trump #matrix","viewCount":"1364099"},
		"streamingData":{"adaptiveFormats":[
			{"itag":136,"url":"` + media.URL + `/v?itag=136","mimeType":"video/mp4; codecs=\"avc1.4D401F\"","bitrate":1,"width":720,"height":1280,"contentLength":"` + strconv.Itoa(len(video)) + `"},
			{"itag":140,"url":"` + media.URL + `/a?itag=140","mimeType":"audio/mp4; codecs=\"mp4a.40.2\"","bitrate":1,"contentLength":"` + strconv.Itoa(len(audio)) + `"}]}}`

	ex := f.extractor(t)
	got, err := ex.ExtractMedia(context.Background(), "https://youtube.com/shorts/UeCMLVAZMc8?si=x")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.URLs) != 1 {
		t.Fatalf("urls = %v", got.URLs)
	}
	path := strings.TrimPrefix(got.URLs[0], "file://")
	defer os.Remove(path)
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	if err != nil || strings.Join(strings.Fields(string(out)), ",") != "video,audio" {
		t.Fatalf("muxed file streams = %q, %v", out, err)
	}
	m := got.Metadata
	if m.Title != "Matrix scene" || m.Author != "Duhek clips" || m.Views != 1364099 || m.Description != "#trump #matrix" || m.Source != "youtube" {
		t.Errorf("metadata = %+v", m)
	}

	// the visitor id is cached; a rejected one is replaced once
	if n := f.visitorCalls.Load(); n != 1 {
		t.Errorf("visitor fetched %d times, want 1", n)
	}
	f.visitor, _ = "CgtWSVNJVE9SXzI%3D", f.acceptedVis.Swap("CgtWSVNJVE9SXzI=")
	if _, err := ex.Metadata(context.Background(), "https://youtube.com/shorts/UeCMLVAZMc8"); err != nil {
		t.Fatalf("after a visitor change: %v", err)
	}
	if n := f.visitorCalls.Load(); n != 2 {
		t.Errorf("visitor fetched %d times, want 2", n)
	}
}

func TestYouTubeStatuses(t *testing.T) {
	f := newFakeYouTube(t)
	f.players["AAAAAAAAAAA"] = `{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"Sign in to confirm your age"}}`
	f.players["BBBBBBBBBBB"] = `{"playabilityStatus":{"status":"OK"},"streamingData":{"adaptiveFormats":[` +
		youtubeFormatJSON(247, `video/webm; codecs="vp9"`, 720, 1280, 1, "vp9", false) + `]}}`
	ex := f.extractor(t)

	// unknown id: the fake says "This video is unavailable"
	if _, err := ex.ExtractMedia(context.Background(), "https://youtube.com/shorts/ZZZZZZZZZZZ"); !errors.Is(err, ErrNoVideo) {
		t.Errorf("removed video: want ErrNoVideo, got %v", err)
	}
	for _, id := range []string{"AAAAAAAAAAA", "BBBBBBBBBBB"} {
		_, err := ex.ExtractMedia(context.Background(), "https://youtube.com/shorts/"+id)
		if err == nil || errors.Is(err, ErrNoVideo) {
			t.Errorf("%s: want a plain error, got %v", id, err)
		}
	}
}

// tinyMP4 makes a one-stream mp4 (a second of black video, or of silence).
func tinyMP4(t *testing.T, kind string) []byte {
	t.Helper()
	path := t.TempDir() + "/" + kind + ".mp4"
	args := []string{"-loglevel", "error", "-f", "lavfi", "-i", "color=black:s=64x64:d=1", "-c:v", "libx264", path}
	if kind == "audio" {
		args = []string{"-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=d=1", "-t", "1", "-c:a", "aac", path}
	}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg can't make test media: %v: %s", err, out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestYouTubeLive extracts real Shorts. YouTube blocks many proxy IPs, and
// media urls only work from the IP that asked, so run it where requests leave
// through one good exit:
//
//	YOUTUBE_LIVE=1 YOUTUBE_LIVE_URLS=https://www.youtube.com/shorts/<id>,... go test ./preview -run TestYouTubeLive -v
func TestYouTubeLive(t *testing.T) {
	if os.Getenv("YOUTUBE_LIVE") == "" {
		t.Skip("set YOUTUBE_LIVE=1 to call YouTube")
	}
	ex, _ := NewYouTube(&url.URL{Scheme: "youtube"})
	for _, u := range strings.Split(os.Getenv("YOUTUBE_LIVE_URLS"), ",") {
		start := time.Now()
		media, err := ex.ExtractMedia(context.Background(), u)
		if err != nil {
			t.Logf("FAIL %s (%s): %v", u, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		path := strings.TrimPrefix(media.URLs[0], "file://")
		streams, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", path).Output()
		st, _ := os.Stat(path)
		os.Remove(path)
		t.Logf("OK %s (%s): %dKB [%s] %q by %s", u, time.Since(start).Round(time.Millisecond), st.Size()/1024,
			strings.Join(strings.Fields(string(streams)), " "), truncate(media.Metadata.Title, 40), media.Metadata.Author)
	}
}
