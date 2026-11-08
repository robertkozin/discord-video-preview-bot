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

// errDownloadStalled means a download stopped receiving data.
var errDownloadStalled = errors.New("download stalled")

// errEmptyMedia means the server answered with an empty body, e.g. a cobalt
// tunnel whose upstream fetch failed.
var errEmptyMedia = errors.New("remote media is empty")

var (
	// downloadIdleTimeout is how long a download may go without receiving a
	// byte, and downloadMaxTime is the longest it may take in total.
	downloadIdleTimeout = 30 * time.Second
	downloadMaxTime     = 5 * time.Minute
)

// Result is what Reupload produced.
type Result struct {
	Links     []string  // permalinks to the reuploaded media
	Metadata  *Metadata // what an extractor knows about the post; nil when none did
	Extractor string    // the extractor that produced the media; empty on a cache hit
	CacheHit  bool
}

type Reuploader struct {
	Extractors  []Extractor
	Destination Destination
	PublicURL   string
}

type Manifest struct {
	CreatedAt time.Time `json:"created_at"`
	SourceURL string    `json:"source_url"`
	Files     []string  `json:"files"`
	Metadata  *Metadata `json:"metadata,omitempty"` // absent in manifests from before it was stored
}

func (reup *Reuploader) IsSupported(mediaURL string) bool {
	for _, extractor := range reup.Extractors {
		if extractor.IsSupported(mediaURL) {
			return true
		}
	}
	return false
}

func (reup *Reuploader) Reupload(ctx context.Context, mediaURL string) (res Result, err error) {
	// also tag the caller's span (discord_reply / discord_command) so a trace
	// can be filtered by site, extractor and cache outcome from its root
	parent := trace.SpanFromContext(ctx)
	ctx, span := tracer.Start(ctx, "reupload")
	defer tr.End(span, &err)
	setAttrs := func(kv ...attribute.KeyValue) {
		span.SetAttributes(kv...)
		parent.SetAttributes(kv...)
	}
	setMetadataAttrs := func(m *Metadata) {
		setAttrs(attribute.Bool("preview.metadata", !m.Empty()))
		if !m.Empty() {
			setAttrs(attribute.String("preview.metadata_source", m.Source))
		}
	}

	cleanURL, err := cleanURLParams(mediaURL, allowedParams)
	if err != nil {
		return Result{}, err
	}
	mediaID := sha12(cleanURL)

	span.SetAttributes(attribute.String("media_url", mediaURL), attribute.String("clean_url", cleanURL), attribute.String("media_id", mediaID))
	setAttrs(attribute.String("preview.site", siteOf(cleanURL)))

	// fast path: video has already been reuploaded
	manifest, err := reup.getManifest(ctx, mediaID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return Result{}, fmt.Errorf("getting manifest: %w", err)
		}
	} else {
		setAttrs(attribute.Bool("preview.cache_hit", true), attribute.Int("preview.files", len(manifest.Files)))
		setMetadataAttrs(manifest.Metadata)
		return Result{Links: reup.formatPermalinks(manifest.Files), Metadata: manifest.Metadata, CacheHit: true}, nil
	}

	// slow path: the first extractor whose media also transfers wins. An
	// extractor can hand out urls that then fail to download (e.g. a cobalt
	// tunnel that answers with an empty body), so a failed transfer moves on to
	// the next extractor instead of failing the reupload.
	setAttrs(attribute.Bool("preview.cache_hit", false))
	var (
		media        Media
		extractor    string
		filenames    []string
		metaCh       chan *Metadata
		transferErrs []error
	)
	for next := 0; ; {
		media, extractor, next, err = reup.extract(ctx, cleanURL, next)
		if err != nil {
			if len(transferErrs) > 0 {
				err = errors.Join(append(transferErrs, err)...)
			}
			return Result{}, fmt.Errorf("extracting: %w", err)
		}
		setAttrs(attribute.String("preview.extractor", extractor))

		// when the extractor that won didn't know the post's details, ask the
		// ones that can while the media transfers
		metaCh = nil
		if media.Metadata.Empty() {
			metaCh = make(chan *Metadata, 1)
			go func(skip string) { metaCh <- reup.fetchMetadata(ctx, cleanURL, skip) }(extractor)
		}

		filenames, err = reup.transferMany(ctx, media.URLs, mediaID)
		if err == nil {
			break
		}
		err = fmt.Errorf("reuploading from %s: %w", extractor, err)
		if ctx.Err() != nil {
			return Result{}, err
		}
		span.AddEvent("transfer_failed", trace.WithAttributes(
			attribute.String("extractor", extractor),
			attribute.String("error", truncate(err.Error(), 500)),
		))
		transferErrs = append(transferErrs, err)
	}
	if len(transferErrs) > 0 {
		setAttrs(attribute.Int("preview.transfer_fallbacks", len(transferErrs)))
	}

	metadata := media.Metadata
	if metaCh != nil {
		metadata = <-metaCh
	}
	if metadata != nil && metadata.Source == "" {
		metadata.Source = extractor
	}
	setMetadataAttrs(metadata)

	manifest = Manifest{
		CreatedAt: time.Now().UTC(),
		SourceURL: cleanURL,
		Files:     filenames,
		Metadata:  metadata,
	}
	err = reup.uploadManifest(ctx, mediaID, manifest)
	if err != nil {
		return Result{}, fmt.Errorf("uploading manifest: %w", err)
	}

	setAttrs(attribute.Int("preview.files", len(manifest.Files)))
	return Result{Links: reup.formatPermalinks(manifest.Files), Metadata: metadata, Extractor: extractor}, nil
}

// metadataTimeout bounds asking providers for a post's details; the reply
// shouldn't wait on them for long.
const metadataTimeout = 10 * time.Second

// fetchMetadata asks the extractors that can describe a post (except skip,
// which just ran) in order, and returns the first answer with anything in it.
// It never fails: a post with no metadata still gets its media.
func (reup *Reuploader) fetchMetadata(ctx context.Context, mediaURL, skip string) *Metadata {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	ctx, span := tracer.Start(ctx, "fetch_metadata")
	defer span.End()

	for _, extractor := range reup.Extractors {
		provider, ok := extractor.(MetadataProvider)
		name := extractorName(extractor)
		if !ok || name == skip || !extractor.IsSupported(mediaURL) {
			continue
		}
		m, err := provider.Metadata(ctx, mediaURL)
		if err != nil {
			span.AddEvent("provider_failed", trace.WithAttributes(
				attribute.String("provider", name),
				attribute.String("error", truncate(err.Error(), 500)),
			))
			continue
		}
		if !m.Empty() {
			span.SetAttributes(attribute.String("metadata.source", name))
			return m
		}
	}
	return nil
}

// extract tries the extractors from index from on, and returns the media, the
// name of the extractor that produced it and the index to continue from when
// that media turns out to be unusable.
func (reup *Reuploader) extract(ctx context.Context, mediaURL string, from int) (media Media, used string, next int, err error) {
	ctx, span := tracer.Start(ctx, "extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.String("site", siteOf(mediaURL)))

	errs := []error{}
	var attempted []string
	defer func() {
		span.SetAttributes(attribute.StringSlice("extractor.attempted", attempted))
	}()
	for i := from; i < len(reup.Extractors); i++ {
		extractor := reup.Extractors[i]
		if extractor.IsSupported(mediaURL) {
			name := extractorName(extractor)
			attempted = append(attempted, name)
			media, err = extractMedia(ctx, extractor, mediaURL)
			if errors.Is(err, ErrNoVideo) {
				span.SetAttributes(attribute.Bool("extractor.no_video", true))
				return Media{}, "", 0, fmt.Errorf("extracting media: %s: %w", mediaURL, err)
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
			return media, name, i + 1, nil
		}
	}

	if len(errs) == 0 {
		if from > 0 {
			return Media{}, "", 0, fmt.Errorf("no more extractors to try: %s", mediaURL)
		}
		return Media{}, "", 0, fmt.Errorf("no extractors matching: %s", mediaURL)
	}

	return Media{}, "", 0, fmt.Errorf("extracting media: %s: %w", mediaURL, errors.Join(errs...))
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

	// The ffmpeg fallback is for media the downloader got but couldn't use. It
	// would only fetch the same url a second time after a failure of the
	// connection itself (too slow, too large, cancelled), which on a metered or
	// slow link doubles the cost and the wait, or after an empty body, which
	// ffmpeg can't open either.
	if errors.Is(err, errMediaTooLarge) || errors.Is(err, errDownloadStalled) || errors.Is(err, errEmptyMedia) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		!strings.HasPrefix(remoteURL, "http") {
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

// downloadHttp downloads remoteURL into memory. It gives up when no data has
// arrived for downloadIdleTimeout, or after downloadMaxTime in total, so a slow
// but steady stream keeps going.
func downloadHttp(ctx context.Context, remoteURL string) ([]byte, error) {
	ctx, cancelTotal := context.WithTimeout(ctx, downloadMaxTime)
	defer cancelTotal()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(downloadIdleTimeout, func() {
		cancel(fmt.Errorf("%w: no data for %s", errDownloadStalled, downloadIdleTimeout))
	})
	defer idle.Stop()

	body, err := downloadHttpBody(ctx, remoteURL, idle)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errDownloadStalled) {
			return nil, cause
		}
		return nil, err
	}
	return body, nil
}

// idleReader pushes the idle timer back on every read that returns data.
type idleReader struct {
	r    io.Reader
	idle *time.Timer
}

func (ir idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.idle.Reset(downloadIdleTimeout)
	}
	return n, err
}

func downloadHttpBody(ctx context.Context, remoteURL string, idle *time.Timer) ([]byte, error) {
	resp, err := httpGetMedia(ctx, remoteURL)
	if err != nil {
		return nil, fmt.Errorf("fetching remote url: %w", err)
	}
	defer resp.Body.Close()
	idle.Reset(downloadIdleTimeout) // headers arrived

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected error fetching remote url: %s", resp.Status)
	}

	if resp.ContentLength > MaxMediaSize {
		return nil, fmt.Errorf("%w: %d bytes", errMediaTooLarge, resp.ContentLength)
	}

	return readAll(idleReader{r: resp.Body, idle: idle}, resp.ContentLength)
}

func readAll(reader io.Reader, size int64) (body []byte, err error) {
	if size > 0 {
		body = make([]byte, size)
		_, err = io.ReadFull(reader, body)
	} else if size < 0 {
		// no Content-Length (e.g. a tunnel): cap it so a runaway stream can't fill memory
		body, err = io.ReadAll(io.LimitReader(reader, MaxMediaSize+1))
		if err == nil && len(body) > MaxMediaSize {
			return nil, fmt.Errorf("%w: more than %d bytes", errMediaTooLarge, MaxMediaSize)
		}
	} else {
		err = errEmptyMedia
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
