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
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

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
	ctx, span := tracer.Start(ctx, "reupload")
	defer tr.End(span, &err)

	cleanURL, err := cleanURLParams(mediaURL, allowedParams)
	if err != nil {
		return nil, err
	}
	mediaID := sha12(cleanURL)

	span.SetAttributes(attribute.String("media_url", mediaURL), attribute.String("clean_url", cleanURL), attribute.String("media_id", mediaID))

	// fast path: video has already been reuploaded
	manifest, err := reup.getManifest(ctx, mediaID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("getting manifest: %w", err)
		}
	} else {
		permalinks := reup.formatPermalinks(manifest.Files)
		return permalinks, nil
	}

	// slow path:
	remoteURLs, err := reup.extract(ctx, cleanURL)
	if err != nil {
		return nil, fmt.Errorf("extracting: %w", err)
	}

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

	permalinks := reup.formatPermalinks(manifest.Files)
	return permalinks, nil
}

func (reup *Reuploader) extract(ctx context.Context, mediaURL string) (remoteURLs []string, err error) {
	ctx, span := tracer.Start(ctx, "extract")
	defer tr.End(span, &err)

	errs := []error{}
	for _, extractor := range reup.Extractors {
		if extractor.IsSupported(mediaURL) {
			remoteURLs, err = extractor.Extract(ctx, mediaURL)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			return remoteURLs, nil
		}
	}

	if len(errs) == 0 {
		return nil, fmt.Errorf("no extractors matching: %s", mediaURL)
	}

	return nil, fmt.Errorf("extracting media: %s: %w", mediaURL, errors.Join(errs...))
}

func (reup *Reuploader) transferMany(ctx context.Context, remoteURLs []string, mediaID string) ([]string, error) {
	ctx, span := tracer.Start(ctx, "transfer_many")
	defer span.End()

	if len(remoteURLs) == 1 {
		name := mediaID
		filename, err := reup.transfer(ctx, remoteURLs[0], name)
		if err != nil {
			return nil, fmt.Errorf("transfering from %s: %w", remoteURLs[0], err)
		}
		return []string{filename}, nil
	}

	filenames := make([]string, 0, len(remoteURLs))
	for i, remoteURL := range remoteURLs {
		name := fmt.Sprintf("%s-%d", mediaID, i+1)
		filename, err := reup.transfer(ctx, remoteURL, name)
		if err != nil {
			continue // TODO, continue but keep track of errors, error if all are errors
		}
		filenames = append(filenames, filename)
	}

	return filenames, nil
}

func (reup *Reuploader) transfer(ctx context.Context, remoteURL string, name string) (string, error) {
	ctx, span := tracer.Start(ctx, "transfer_one")
	defer span.End()

	resp, err := httpGet(ctx, remoteURL)
	if err != nil {
		return "", fmt.Errorf("fetching remote url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("unexpected error fetching remote url: %s", resp.Status)
	}

	if resp.ContentLength > MaxMediaSize {
		return "", fmt.Errorf("remote media is too large: %dbytes", resp.ContentLength)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("downloading media to memory: %w", err)
	} else if body == nil || len(body) == 0 {
		return "", fmt.Errorf("expecting media response body to not be empty")
	}

	contentType := http.DetectContentType(body)
	if !slices.Contains(allowedMediaTypes, contentType) {
		return "", fmt.Errorf("expecting allowed content type: %s", contentType)
	}

	ext := extensionByType[contentType]

	filename := name + ext

	err = reup.Destination.Upload(ctx, filename, body)
	if err != nil {
		return "", fmt.Errorf("uploading: %w", err)
	}

	return filename, nil
}

func (reup *Reuploader) getManifest(ctx context.Context, mediaID string) (Manifest, error) {
	ctx, span := tracer.Start(ctx, "get_manifest")
	defer span.End()

	name := mediaID + ".json"
	manifestBytes, err := reup.Destination.Download(ctx, name)
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

func (reup *Reuploader) uploadManifest(ctx context.Context, mediaID string, manifest Manifest) error {
	ctx, span := tracer.Start(ctx, "upload_manifest")
	defer span.End()

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
