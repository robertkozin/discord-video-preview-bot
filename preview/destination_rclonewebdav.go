package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
)

var _ Destination = (*RCloneWebDAVDestination)(nil)

type RCloneWebDAVDestination struct {
	baseURL string
}

func NewRCloneWebDAV(ctx context.Context, config *url.URL) (*RCloneWebDAVDestination, error) {
	base := *config
	base.Scheme = "http"

	return &RCloneWebDAVDestination{
		baseURL: base.String(),
	}, nil
}

func (w *RCloneWebDAVDestination) String() string {
	return fmt.Sprintf("rclone+webdav: %q", w.baseURL)
}

func (r *RCloneWebDAVDestination) Close() error {
	return nil
}

func (r *RCloneWebDAVDestination) Download(ctx context.Context, name string) ([]byte, error) {
	ctx, span := tracer.Start(ctx, "rclone+webdav_download")
	defer span.End()

	if err := validateSimpleFilename(name); err != nil {
		return nil, err
	}

	fileURL := urlCat(r.baseURL, name)

	// resp, err := httpGet(ctx, fileURL)
	resp, err := http.Get(fileURL)
	if err != nil {
		return nil, fmt.Errorf("downloading file from rclone+webdav: %w", err)
	}
	defer resp.Body.Close()

	if resp.ContentLength > MaxMediaSize {
		return nil, errors.New("media too large")
	} else if resp.ContentLength <= 0 {
		return nil, errors.New("media is empty")
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, fs.ErrNotExist
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected response status: %s", resp.Status)
	}

	content := make([]byte, resp.ContentLength)
	_, err = io.ReadFull(resp.Body, content)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return content, nil
}

func (r *RCloneWebDAVDestination) Upload(ctx context.Context, name string, content []byte) error {
	ctx, span := tracer.Start(ctx, "rclone+webdav_upload")
	defer span.End()

	if err := validateSimpleFilename(name); err != nil {
		return err
	}

	fileURL := urlCat(r.baseURL, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, fileURL, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("creating upload request: %w", err)
	}
	req.ContentLength = int64(len(content))

	//resp, err := httpDo(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("uploading file to rclone+webdav: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %s", resp.Status)
	}

	return nil
}
