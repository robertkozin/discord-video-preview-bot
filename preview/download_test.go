package preview

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withDownloadTimeouts shortens the download timeouts for a test.
func withDownloadTimeouts(t *testing.T, idle, total time.Duration) {
	t.Helper()
	oldIdle, oldTotal := downloadIdleTimeout, downloadMaxTime
	downloadIdleTimeout, downloadMaxTime = idle, total
	t.Cleanup(func() { downloadIdleTimeout, downloadMaxTime = oldIdle, oldTotal })
}

// A slow stream that keeps sending data (like a tunnel over a mobile proxy,
// with no Content-Length) must finish even when it takes longer than the idle
// timeout in total.
func TestDownloadSlowButSteadyStream(t *testing.T) {
	withDownloadTimeouts(t, 400*time.Millisecond, 10*time.Second)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 15; i++ {
			_, _ = w.Write([]byte("0123456789"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	start := time.Now()
	body, err := downloadHttp(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if len(body) != 150 {
		t.Errorf("got %d bytes, want 150", len(body))
	}
	if time.Since(start) < 1200*time.Millisecond {
		t.Errorf("finished in %s, expected a slow stream", time.Since(start))
	}
}

// A stream that stops sending is cut off after the idle timeout, and fetchMedia
// must not retry it through ffmpeg.
func TestDownloadStallIsNotRetriedWithFFmpeg(t *testing.T) {
	withDownloadTimeouts(t, 300*time.Millisecond, 10*time.Second)

	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // send nothing more
	}))
	defer srv.Close()

	start := time.Now()
	_, _, err := fetchMedia(context.Background(), srv.URL)
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("want errDownloadStalled, got %v", err)
	}
	if strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("stall was retried with ffmpeg: %v", err)
	}
	if requests != 1 {
		t.Errorf("url fetched %d times, want 1", requests)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %s to give up", time.Since(start))
	}
}

// The total limit still applies to a stream that keeps going forever.
func TestDownloadTotalLimit(t *testing.T) {
	withDownloadTimeouts(t, time.Second, 500*time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for r.Context().Err() == nil {
			_, _ = w.Write([]byte("x"))
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	_, err := downloadHttp(context.Background(), srv.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
}
