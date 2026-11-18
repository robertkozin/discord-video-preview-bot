package preview

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// staticExtractor hands out fixed urls for every link.
type staticExtractor struct {
	urls  []string
	calls int
}

func (s *staticExtractor) IsSupported(string) bool { return true }

func (s *staticExtractor) Extract(context.Context, string) ([]string, error) {
	s.calls++
	return s.urls, nil
}

// pngBody is enough of a png for content sniffing.
const pngBody = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"

// An empty body (a cobalt tunnel whose upstream failed) is not retried through
// ffmpeg, which would only fetch the same empty url again.
func TestEmptyBodyIsNotRetriedWithFFmpeg(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Length", "0")
	}))
	defer srv.Close()

	_, _, err := fetchMedia(context.Background(), srv.URL)
	if !errors.Is(err, errEmptyMedia) {
		t.Fatalf("want errEmptyMedia, got %v", err)
	}
	if strings.Contains(err.Error(), "ffmpeg") || requests != 1 {
		t.Errorf("empty body was retried (%d requests): %v", requests, err)
	}
}

// When the first extractor's media can't be downloaded, the next extractor gets
// its turn instead of the reupload failing.
func TestReuploadFallsBackWhenTransferFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty" {
			w.Header().Set("Content-Length", "0")
			return
		}
		_, _ = w.Write([]byte(pngBody))
	}))
	defer srv.Close()

	broken := &staticExtractor{urls: []string{srv.URL + "/empty"}}
	working := &staticExtractor{urls: []string{srv.URL + "/ok"}}
	reup := &Reuploader{
		Extractors:  []Extractor{broken, working},
		Destination: DiscardDestination{},
		PublicURL:   "https://example.test/",
	}

	res, err := reup.Reupload(context.Background(), "https://example.test/post/1")
	if err != nil {
		t.Fatalf("reupload failed: %v", err)
	}
	if broken.calls != 1 || working.calls != 1 {
		t.Errorf("extractor calls: broken=%d working=%d, want 1 each", broken.calls, working.calls)
	}
	if len(res.Links) != 1 || !strings.HasSuffix(res.Links[0], ".png") {
		t.Errorf("links = %v, want one png", res.Links)
	}
}

// When every extractor's media fails to download, the error says so for each.
func TestReuploadAllTransfersFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
	}))
	defer srv.Close()

	reup := &Reuploader{
		Extractors: []Extractor{
			&staticExtractor{urls: []string{srv.URL + "/a"}},
			&staticExtractor{urls: []string{srv.URL + "/b"}},
		},
		Destination: DiscardDestination{},
	}
	_, err := reup.Reupload(context.Background(), "https://example.test/post/2")
	if !errors.Is(err, errEmptyMedia) {
		t.Fatalf("want errEmptyMedia, got %v", err)
	}
	if !strings.Contains(err.Error(), "/a") || !strings.Contains(err.Error(), "/b") {
		t.Errorf("error should mention both transfers: %v", err)
	}
}

// A 204 No Content (ssstik's tikcdn.io for a post it couldn't get) is empty
// media, not retried through ffmpeg.
func TestNoContentIsEmptyMedia(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, _, err := fetchMedia(context.Background(), srv.URL)
	if !errors.Is(err, errEmptyMedia) {
		t.Fatalf("want errEmptyMedia, got %v", err)
	}
	if requests != 1 {
		t.Errorf("204 was retried (%d requests): %v", requests, err)
	}
}

// A webp image (Instagram posts whose photos only come as webp) is stored as
// a jpeg.
func TestWebpIsConvertedToJPEG(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	webp, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x48:duration=1",
		"-frames:v", "1", "-c:v", "libwebp", "-f", "webp", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg can't encode webp: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(webp)
	}))
	defer srv.Close()

	body, contentType, err := fetchMedia(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "image/jpeg" || detectContentType(body) != "image/jpeg" {
		t.Errorf("got %s (sniffed %s), want image/jpeg", contentType, detectContentType(body))
	}
}

// An image the reuploader can't use ends the reupload: the other extractors
// would hand out the same image.
func TestUnsupportedImageStopsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	}))
	defer srv.Close()

	first := &staticExtractor{urls: []string{srv.URL + "/a"}}
	second := &staticExtractor{urls: []string{srv.URL + "/b"}}
	reup := &Reuploader{Extractors: []Extractor{first, second}, Destination: DiscardDestination{}}
	_, err := reup.Reupload(context.Background(), "https://example.test/post/3")
	if !errors.Is(err, errUnsupportedMedia) {
		t.Fatalf("want errUnsupportedMedia, got %v", err)
	}
	if second.calls != 0 {
		t.Errorf("second extractor was tried")
	}
	if strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("image was remuxed with ffmpeg: %v", err)
	}
}

func TestYtDlpError(t *testing.T) {
	stderr := "WARNING: [youtube] something\nERROR: [youtube] abc: The uploader has not made this video available in your country\nYou might want to use a VPN or a proxy server (with --proxy) to workaround.\n"
	want := "ERROR: [youtube] abc: The uploader has not made this video available in your country You might want to use a VPN or a proxy server (with --proxy) to workaround."
	if got := ytDlpError(stderr); got != want {
		t.Errorf("got %q", got)
	}
	if got := ytDlpError("just one line\n"); got != "just one line" {
		t.Errorf("no ERROR line: got %q", got)
	}
}
