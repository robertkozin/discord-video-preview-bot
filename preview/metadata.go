package preview

import (
	"context"
	"strings"
	"time"
)

// Metadata is what an extractor knows about a post besides its media, for the
// reply's text. Every field is optional.
type Metadata struct {
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"` // the post's text or caption
	Author       string `json:"author,omitempty"`      // display name
	AuthorHandle string `json:"author_handle,omitempty"`

	Likes    int64     `json:"likes,omitempty"`
	Views    int64     `json:"views,omitempty"`
	Comments int64     `json:"comments,omitempty"`
	Shares   int64     `json:"shares,omitempty"` // reposts, retweets
	PostedAt time.Time `json:"posted_at,omitzero"`

	// Source names the extractor that provided it.
	Source string `json:"source,omitempty"`
}

// Empty reports whether m has nothing to put in a reply's text: no title,
// description or author. Stats alone don't count.
func (m *Metadata) Empty() bool {
	return m == nil || (clean(m.Title) == "" && clean(m.Description) == "" && clean(m.Author) == "" && clean(m.AuthorHandle) == "")
}

func clean(s string) string { return strings.TrimSpace(s) }

// Media is what an extractor found: where to download from, and optionally
// what it knows about the post.
type Media struct {
	URLs     []string
	Metadata *Metadata
}

// MediaExtractor is an optional interface for an Extractor that learns about
// the post while extracting its media, so the reuploader doesn't have to ask
// again. Extract on the same type should return just the URLs.
type MediaExtractor interface {
	Extractor
	ExtractMedia(ctx context.Context, mediaURL string) (Media, error)
}

// MetadataProvider is an optional interface for an Extractor that can describe
// a post without extracting its media. When the extractor that supplied the
// media had no metadata, the reuploader asks providers (in extractor order)
// for it while the media transfers.
type MetadataProvider interface {
	Extractor
	Metadata(ctx context.Context, mediaURL string) (*Metadata, error)
}

// extractMedia runs ex, using ExtractMedia when it has one.
func extractMedia(ctx context.Context, ex Extractor, mediaURL string) (Media, error) {
	if me, ok := ex.(MediaExtractor); ok {
		return me.ExtractMedia(ctx, mediaURL)
	}
	urls, err := ex.Extract(ctx, mediaURL)
	return Media{URLs: urls}, err
}
