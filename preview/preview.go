package preview

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/url"

	"go.opentelemetry.io/otel"
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

func NewExtractor(config *url.URL) (ex Extractor, err error) {
	switch config.Scheme {
	case "cobalt":
		ex, err = NewCobalt(config)
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
