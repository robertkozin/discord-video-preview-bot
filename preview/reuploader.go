package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var errMediaTooLarge = errors.New("remote media is too large")

type Reuploader struct {
	Extractors  []Extractor
	Destination Destination
	PublicURL   string
}

type Manifest struct {
	CreatedAt time.Time `json:"created_at"`
	SourceURL string    `json:"source_url"`
	Files     []string  `json:"files"`
}

func (reup *Reuploader) IsSupported(mediaURL string) bool {
	for _, extractor := range reup.Extractors {
		if extractor.IsSupported(mediaURL) {
			return true
		}
	}
	return false
}

func (reup *Reuploader) Reupload(ctx context.Context, mediaURL string) ([]string, error) {
	var err error
	// also tag the caller's span (discord_reply / discord_command) so a trace
	// can be filtered by site, extractor and cache outcome from its root
	parent := trace.SpanFromContext(ctx)
	ctx, span := tracer.Start(ctx, "reupload")
	defer tr.End(span, &err)
	setAttrs := func(kv ...attribute.KeyValue) {
		span.SetAttributes(kv...)
		parent.SetAttributes(kv...)
	}

	cleanURL, err := cleanURLParams(mediaURL, allowedParams)
	if err != nil {
		return nil, err
	}
	mediaID := sha12(cleanURL)

	span.SetAttributes(attribute.String("media_url", mediaURL), attribute.String("clean_url", cleanURL), attribute.String("media_id", mediaID))
	setAttrs(attribute.String("preview.site", siteOf(cleanURL)))

	// fast path: video has already been reuploaded
	manifest, err := reup.getManifest(ctx, mediaID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("getting manifest: %w", err)
		}
	} else {
		setAttrs(attribute.Bool("preview.cache_hit", true), attribute.Int("preview.files", len(manifest.Files)))
		permalinks := reup.formatPermalinks(manifest.Files)
		return permalinks, nil
	}

	// slow path:
	setAttrs(attribute.Bool("preview.cache_hit", false))
	remoteURLs, extractor, err := reup.extract(ctx, cleanURL)
	if err != nil {
		return nil, fmt.Errorf("extracting: %w", err)
	}
	setAttrs(attribute.String("preview.extractor", extractor))

	filenames, err := reup.transferMany(ctx, remoteURLs, mediaID)
	if err != nil {
		return nil, fmt.Errorf("reuploading: %w", err)
	}

	manifest = Manifest{
		CreatedAt: time.Now().UTC(),
		SourceURL: cleanURL,
		Files:     filenames,
	}
	err = reup.uploadManifest(ctx, mediaID, manifest)
	if err != nil {
		return nil, fmt.Errorf("uploading manifest: %w", err)
	}

	setAttrs(attribute.Int("preview.files", len(manifest.Files)))
	permalinks := reup.formatPermalinks(manifest.Files)
	return permalinks, nil
}

// extract returns the remote urls and the name of the extractor that produced them.
func (reup *Reuploader) extract(ctx context.Context, mediaURL string) (remoteURLs []string, used string, err error) {
	ctx, span := tracer.Start(ctx, "extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	errs := []error{}
	var attempted []string
	defer func() {
		span.SetAttributes(attribute.StringSlice("extractor.attempted", attempted))
	}()
	for _, extractor := range reup.Extractors {
		if extractor.IsSupported(mediaURL) {
			name := extractorName(extractor)
			attempted = append(attempted, name)
			remoteURLs, err = extractor.Extract(ctx, mediaURL)
			if errors.Is(err, ErrNoVideo) {
				span.SetAttributes(attribute.Bool("extractor.no_video", true))
				return nil, "", fmt.Errorf("extracting media: %s: %w", mediaURL, err)
			}
			if err != nil {
				span.AddEvent("extractor_failed", trace.WithAttributes(
					attribute.String("extractor", name),
					attribute.String("error", truncate(err.Error(), 500)),
				))
				errs = append(errs, err)
				continue
			}
			span.SetAttributes(attribute.String("extractor.used", name))
			return remoteURLs, name, nil
		}
	}

	if len(errs) == 0 {
		return nil, "", fmt.Errorf("no extractors matching: %s", mediaURL)
	}

	return nil, "", fmt.Errorf("extracting media: %s: %w", mediaURL, errors.Join(errs...))
}

func (reup *Reuploader) transferMany(ctx context.Context, remoteURLs []string, mediaID string) (_ []string, err error) {
	ctx, span := tracer.Start(ctx, "transfer_many")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.Int("transfer.count", len(remoteURLs)))

	if len(remoteURLs) == 1 {
		name := mediaID
		filename, err := reup.transfer(ctx, remoteURLs[0], name)
		if err != nil {
			return nil, fmt.Errorf("transfering from %s: %w", remoteURLs[0], err)
		}
		return []string{filename}, nil
	}

	filenames := make([]string, 0, len(remoteURLs))
	var errs []error
	for i, remoteURL := range remoteURLs {
		name := fmt.Sprintf("%s-%d", mediaID, i+1)
		filename, err := reup.transfer(ctx, remoteURL, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		filenames = append(filenames, filename)
	}
	span.SetAttributes(attribute.Int("transfer.failed", len(errs)))

	// partial success still works, but all failing used to look like success
	if len(filenames) == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return filenames, nil
}

func (reup *Reuploader) transfer(ctx context.Context, remoteURL string, name string) (_ string, err error) {
	ctx, span := tracer.Start(ctx, "transfer_one", trace.WithAttributes(attribute.String("remote_url", redactRawURL(remoteURL))))
	defer tr.End(span, &err)

	body, contentType, err := fetchMedia(ctx, remoteURL)
	if err != nil {
		return "", err
	}
	span.SetAttributes(attribute.Int("media.bytes", len(body)), attribute.String("media.content_type", contentType))

	ext := extensionByType[contentType]

	filename := name + ext

	err = reup.Destination.Upload(ctx, filename, body)
	if err != nil {
		return "", fmt.Errorf("uploading: %w", err)
	}

	return filename, nil
}

// fetchMedia downloads remoteURL into memory. When the go downloader doesn't
// produce allowed media (odd containers, HLS playlists, etc.) it falls back to
// letting ffmpeg remux the url into an mp4.
func fetchMedia(ctx context.Context, remoteURL string) ([]byte, string, error) {
	body, err := download(ctx, remoteURL)
	if err == nil {
		contentType := detectContentType(body)
		if slices.Contains(allowedMediaTypes, contentType) {
			return body, contentType, nil
		}
		err = fmt.Errorf("expecting allowed content type: %s", contentType)
	} else {
		err = fmt.Errorf("downloading media to memory: %w", err)
	}

	if errors.Is(err, errMediaTooLarge) || !strings.HasPrefix(remoteURL, "http") {
		return nil, "", err
	}

	fileURL, muxErr := muxToMP4(ctx, remoteURL)
	if muxErr != nil {
		return nil, "", errors.Join(err, fmt.Errorf("ffmpeg fallback: %w", muxErr))
	}
	body, muxErr = download(ctx, fileURL)
	if muxErr != nil {
		return nil, "", errors.Join(err, fmt.Errorf("ffmpeg fallback: %w", muxErr))
	}
	return body, "video/mp4", nil
}

func download(ctx context.Context, remoteURL string) (_ []byte, err error) {
	ctx, span := tracer.Start(ctx, "download")
	defer tr.End(span, &err)

	parsedURL, err := url.Parse(remoteURL)
	if err != nil {
		return nil, fmt.Errorf("parsing remote url: %w", err)
	}

	switch parsedURL.Scheme {
	case "https", "http":
		return downloadHttp(ctx, remoteURL)
	case "file":
		return downloadFile(parsedURL.Path)
	default:
		return nil, fmt.Errorf("expecting http(s):// or file:// for remote media url: %s", parsedURL.String())
	}
}

func downloadFile(path string) ([]byte, error) {
	if isTempMediaFile(path) {
		defer os.Remove(path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("getting file info: %w", err)
	}

	return readAll(file, stat.Size())
}

func downloadHttp(ctx context.Context, remoteURL string) ([]byte, error) {
	resp, err := httpGet(ctx, remoteURL)
	if err != nil {
		return nil, fmt.Errorf("fetching remote url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected error fetching remote url: %s", resp.Status)
	}

	if resp.ContentLength > MaxMediaSize {
		return nil, fmt.Errorf("%w: %d bytes", errMediaTooLarge, resp.ContentLength)
	}

	return readAll(resp.Body, resp.ContentLength)
}

func readAll(reader io.Reader, size int64) (body []byte, err error) {
	if size > 0 {
		body = make([]byte, size)
		_, err = io.ReadFull(reader, body)
	} else if size < 0 {
		body, err = io.ReadAll(reader)
	} else {
		err = fmt.Errorf("read all: expecting a non-zero size")
	}

	return body, err
}

func (reup *Reuploader) getManifest(ctx context.Context, mediaID string) (_ Manifest, err error) {
	ctx, span := tracer.Start(ctx, "get_manifest")
	// a missing manifest is a cache miss, not a failure
	defer tr.EndIgnore(span, &err, fs.ErrNotExist)

	name := mediaID + ".json"
	manifestBytes, err := reup.Destination.Download(ctx, name)
	span.SetAttributes(attribute.Bool("cache.hit", err == nil))
	if err != nil {
		return Manifest{}, fmt.Errorf("downloading manifest: %w", err)
	}
	var manifest Manifest
	err = json.Unmarshal(manifestBytes, &manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("unmarshaling manifest: %w", err)
	}
	return manifest, nil
}

func (reup *Reuploader) uploadManifest(ctx context.Context, mediaID string, manifest Manifest) (err error) {
	ctx, span := tracer.Start(ctx, "upload_manifest")
	defer tr.End(span, &err)

	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling manifest: %w", err)
	}
	name := mediaID + ".json"
	err = reup.Destination.Upload(ctx, name, b)
	if err != nil {
		return fmt.Errorf("uploading manifest: %w", err)
	}
	return nil
}

func (reup *Reuploader) formatPermalinks(files []string) []string {
	permalinks := make([]string, len(files))
	for i, file := range files {
		permalinks[i] = urlCat(reup.PublicURL, file)
	}
	return permalinks
}

func urlCat(a, b string) string {
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

func sha12(s string) string {
	hash := sha256.Sum256([]byte(s))
	shortHash := hex.EncodeToString(hash[:])[:12] // 12 chars
	return shortHash
}

var allowedParams = map[string]map[string]bool{
	"youtube.com": {"v": true, "t": true},
}

func cleanURLParams(rawURL string, allowList map[string]map[string]bool) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	allowed := allowList[u.Hostname()]
	query := u.Query()
	filtered := url.Values{}

	for param, values := range query {
		if allowed[param] {
			filtered[param] = values
		}
	}

	u.RawQuery = filtered.Encode()
	u.Fragment = ""
	return u.String(), nil
}
