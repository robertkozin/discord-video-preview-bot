package preview

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/url"
	"path/filepath"

	blazer "github.com/Backblaze/blazer/b2"
)

var _ Destination = (*B2Destination)(nil)

type B2Destination struct {
	bucket *blazer.Bucket
}

func NewB2(ctx context.Context, config *url.URL) (*B2Destination, error) {
	keyID := config.User.Username()
	appKey, _ := config.User.Password()
	bucketName := config.Hostname()

	client, err := blazer.NewClient(ctx, keyID, appKey)
	if err != nil {
		return nil, fmt.Errorf("creating blazer/b2 client: %w", err)
	}
	bucket, err := client.Bucket(ctx, bucketName)
	if err != nil {
		return nil, fmt.Errorf("getting b2 bucket: %w", err)
	}

	return &B2Destination{bucket: bucket}, nil
}

func (b2 *B2Destination) String() string {
	return fmt.Sprintf("b2 %q bucket", b2.bucket.Name())
}

func (b2 *B2Destination) Close() error {
	return nil
}

func (b2 *B2Destination) Download(ctx context.Context, name string) ([]byte, error) {
	r := b2.bucket.Object(name).NewReader(ctx)
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		if blazer.IsNotExist(err) {
			return nil, fs.ErrNotExist // TODO what to I do here. io, not found?? yeah think so
		}
		return nil, fmt.Errorf("reading manifest from b2: %w", err)
	}

	return b, nil
}

func (b2 *B2Destination) Upload(ctx context.Context, name string, content []byte) error {
	if err := validateSimpleFilename(name); err != nil {
		return err
	}

	obj := b2.bucket.Object(name)
	uploadAttrs := blazer.Attrs{}

	// shouldUpload, err := b2.shouldUpload(ctx, obj, name)
	// if err != nil {
	// 	return fmt.Errorf("determining whether to upload file to b2: %w", err)
	// } else if !shouldUpload {
	// 	return nil
	// }

	{
		ext := filepath.Ext(name)
		if ext == "" {
			return fmt.Errorf("expecting extension for b2 file upload: %s", name)
		}
		mimeType := mime.TypeByExtension(ext)
		if mimeType == "" {
			return fmt.Errorf("expecting to find mime type for b2 file upload extension: %s", name)
		}
		uploadAttrs.ContentType = mimeType
	}

	writer := obj.NewWriter(ctx, blazer.WithAttrsOption(&uploadAttrs))
	writer.ChunkSize = MaxMediaSize + 1
	writer.UseFileBuffer = false

	reader := bytes.NewReader(content)
	_, err := writer.ReadFrom(reader)
	if err != nil {
		writer.Close()
		return fmt.Errorf("copying file to b2: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing b2 file: %w", err)
	}

	return nil
}

// func (b2 *B2Destination) shouldUpload(ctx context.Context, obj *blazer.Object, data []byte) (bool, error) {
// 	if file.Size == 0 && file.ModTime.IsZero() && file.SHA1 == "" {
// 		return true, nil
// 	}

// 	remote, err := obj.Attrs(ctx)
// 	if err != nil {
// 		if blazer.IsNotExist(err) {
// 			return true, nil
// 		}
// 		return true, fmt.Errorf("getting b2 obj attributes: %w", err)
// 	}

// 	if file.SHA1 != "" && remote.SHA1 != "" && remote.SHA1 != "none" {
// 		return file.SHA1 != remote.SHA1, nil
// 	}

// 	if !file.ModTime.IsZero() && remote.LastModified.After(file.ModTime) {
// 		return false, nil
// 	}

// 	if file.Size != 0 && file.Size == remote.Size {
// 		return false, nil
// 	}

// 	return true, nil
// }
