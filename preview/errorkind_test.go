package preview

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestErrorKind(t *testing.T) {
	for msg, want := range map[string]string{
		"instagram media info: 400 Bad Request: geoblock_required":         kindGeoblock,
		"instagram session rejected (login required): 401: login_required": kindLoginRequired,
		"youtube: LOGIN_REQUIRED: Sign in to confirm you're not a bot":     kindBotCheck,
		"x embed: post has a card but no media":                            kindNoMedia,
		"tiktok: status 10231 cross_border_violation":                      kindGeoblock,
		"tiktok: status 10216 private":                                     kindPrivate,
		"cobalt: 429 Too Many Requests":                                    kindRateLimited,
		"youtube: UNPLAYABLE: This video is not available in your country": kindGeoblock,
		"tiktok: age-restricted post needs a login":                        kindLoginRequired,
		"tiktok page: 403 Forbidden: denied":                               kindHTTP,
		"tiktok: no jpeg for image 3":                                      kindNoMedia,
		"tiktok id 7710231999 ok":                                          kindOther,
		"something odd":                                                    kindOther,
	} {
		if got := errorKind(errors.New(msg)); got != want {
			t.Errorf("errorKind(%q) = %q, want %q", msg, got, want)
		}
	}
	if got := errorKind(fmt.Errorf("x: %w", context.DeadlineExceeded)); got != kindTimeout {
		t.Errorf("deadline: got %q", got)
	}
}

// A failed extractor is recorded on its span without an error status, a
// no-video answer and a success leave it ok.
func TestEndExtractor(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	for name, err := range map[string]error{"fail": errors.New("boom"), "novideo": ErrNoVideo, "ok": nil} {
		_, span := tp.Tracer("t").Start(context.Background(), name)
		e := err
		EndExtractor(span, &e)
	}
	for _, s := range rec.Ended() {
		failed := false
		for _, a := range s.Attributes() {
			if a.Key == "extractor.failed" {
				failed = a.Value.AsBool()
			}
		}
		if s.Status().Code == codes.Error {
			t.Errorf("%s: status is error", s.Name())
		}
		if failed != (s.Name() == "fail") {
			t.Errorf("%s: extractor.failed = %v", s.Name(), failed)
		}
	}
}
