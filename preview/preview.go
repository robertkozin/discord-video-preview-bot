package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	megaByte     = 1024 * 1024
	MaxMediaSize = megaByte * 500
)

var (
	tracer = otel.Tracer("preview")

	allowedMediaTypes = []string{"video/mp4", "image/jpeg", "image/png", "image/jpeg"}
	extensionByType   = map[string]string{
		"video/mp4":  ".mp4",
		"image/jpeg": ".jpeg",
		"image/png":  ".png",
		"image/gif":  ".gif",
	}
	typeByExtension = map[string]string{
		".mp4":  "video/mp4",
		".jpeg": "image/jpeg",
		".jpg":  "image/jpeg",
		".gif":  "image/gif",
	}
)

func init() {
	mime.AddExtensionType(".mp4", "video/mp4")
	mime.AddExtensionType(".jpg", "image/jpeg")
	mime.AddExtensionType(".jpeg", "image/jpeg")
	mime.AddExtensionType(".png", "image/png")
	mime.AddExtensionType(".gif", "image/gif")
	mime.AddExtensionType(".webp", "image/webp")
	mime.AddExtensionType(".webm", "video/webm")
	mime.AddExtensionType(".mp3", "audio/mpeg")
	mime.AddExtensionType(".wav", "audio/wav")
	mime.AddExtensionType(".ogg", "audio/ogg")
}

// ErrNoVideo is returned (wrapped) by an extractor when it is sure the link
// has nothing for any extractor to download, e.g. a text-only tweet. Like
// fs.SkipAll, it stops the reuploader from trying the remaining extractors.
var ErrNoVideo = errors.New("link has no video")

// EndSpan closes a span like tr.End, except that ErrNoVideo is an expected
// outcome rather than a failure: the span keeps an ok status and gets a
// no_video attribute and event instead, so error searches only show real
// failures.
func EndSpan(span trace.Span, err *error) {
	if err != nil && *err != nil && errors.Is(*err, ErrNoVideo) {
		span.SetAttributes(attribute.Bool("no_video", true))
		span.AddEvent("no_video", trace.WithAttributes(attribute.String("reason", truncate((*err).Error(), 500))))
	}
	tr.EndIgnore(span, err, ErrNoVideo)
}

// EndExtractor closes an extractor's span. An extractor that fails isn't a
// failed request when a later extractor then succeeds, so its failure is
// recorded on the span (exception event, extractor.failed, error.kind) but
// leaves the status unset instead of error: error searches and has_errors then
// only show requests that failed altogether, which the extract, reupload and
// root spans report. ErrNoVideo is handled as in EndSpan.
func EndExtractor(span trace.Span, err *error) {
	if err != nil && *err != nil && errors.Is(*err, ErrNoVideo) {
		EndSpan(span, err)
		return
	}
	endQuietly(span, err, "extractor.failed")
}

// endQuietly closes span, recording a failure (exception event, failedAttr,
// error.kind) without an error status: the caller decides whether the failure
// ends the request.
func endQuietly(span trace.Span, err *error, failedAttr string) {
	defer span.End()
	if err != nil && *err != nil {
		span.RecordError(*err)
		span.SetAttributes(attribute.Bool(failedAttr, true), attribute.String("error.kind", errorKind(*err)))
		span.SetStatus(codes.Unset, "")
		return
	}
	span.SetStatus(codes.Ok, "")
}

type Extractor interface {
	IsSupported(mediaURL string) (ok bool)
	Extract(ctx context.Context, mediaURL string) (remoteURLs []string, err error)
}

type Destination interface {
	io.Closer
	fmt.Stringer
	Upload(ctx context.Context, name string, content []byte) error
	Download(ctx context.Context, name string) ([]byte, error)
}

// extractorName is a short, stable name for trace attributes, e.g. "cobalt".
// An extractor that can be configured more than once (one per host) names
// itself with a Name method, e.g. "instafix:eeinstagram.com".
func extractorName(ex Extractor) string {
	if named, ok := ex.(interface{ Name() string }); ok {
		return named.Name()
	}
	name := strings.TrimPrefix(fmt.Sprintf("%T", ex), "*preview.")
	name = strings.TrimSuffix(name, "Extractor")
	return strings.ToLower(name)
}

func NewExtractor(config *url.URL) (ex Extractor, err error) {
	switch config.Scheme {
	case "cobalt":
		ex, err = NewCobalt(config)
	case "brightdata":
		ex, err = NewBrightData(config)
	case "ytdlp":
		ex, err = NewYtDlp(config)
	case "x":
		ex, err = NewX(config)
	case "fxtwitter":
		ex, err = NewFxTwitter(config)
	case "tiktok":
		ex, err = NewTikTok(config)
	case "instagram":
		ex, err = NewInstagram(config)
	case "instagramembed":
		ex, err = NewInstagramEmbed(config)
	case "youtube":
		ex, err = NewYouTube(config)
	case "tikwm":
		ex, err = NewTikWM(config)
	case "instafix":
		ex, err = NewInstaFix(config)
	case "reddit":
		ex, err = NewReddit(config)
	// browser-scraping extractors take no options
	case "fastdl":
		ex = &FastDLExtractor{}
	case "ssstik":
		ex = &SSSTikExtractor{}
	case "reelsvideo":
		ex = &ReelsVideoExtractor{}
	default:
		err = fmt.Errorf("unknown extractor: %s", config.Scheme)
	}
	return ex, err
}

func NewDestination(ctx context.Context, config *url.URL) (dest Destination, err error) {
	switch config.Scheme {
	case "b2":
		dest, err = NewB2(ctx, config)
	case "fs":
		dest, err = NewFSDestination(ctx, config)
	case "rclone+webdav":
		dest, err = NewRCloneWebDAV(ctx, config)
	default:
		err = fmt.Errorf("unknown destination: %s", config.Scheme)
	}
	return dest, err
}
