package preview

import (
	"context"
	"io/fs"
)

var _ Destination = DiscardDestination{}

// DiscardDestination throws uploads away and never has anything to download,
// so a Reuploader backed by it always takes the slow path (every extractor and
// the full media download run) without storing anything. It is for testing.
type DiscardDestination struct{}

func (DiscardDestination) Close() error   { return nil }
func (DiscardDestination) String() string { return "discard" }

func (DiscardDestination) Upload(ctx context.Context, name string, content []byte) error {
	return nil
}

func (DiscardDestination) Download(ctx context.Context, name string) ([]byte, error) {
	return nil, fs.ErrNotExist
}
