package bot

import (
	"strings"
	"testing"

	"github.com/robertkozin/discord-video-preview-bot/preview"
)

func TestMetadataText(t *testing.T) {
	tests := []struct {
		name string
		m    *preview.Metadata
		want string
	}{
		{"nil", nil, ""},
		{"stats only", &preview.Metadata{Likes: 5}, ""},
		{"x post", &preview.Metadata{Author: "Jack", AuthorHandle: "jack", Description: "just setting up my twttr"}, "**Jack** just setting up my twttr"},
		{"title wins over author", &preview.Metadata{Title: "A title", Author: "Someone", Description: "caption"}, "**A title** caption"},
		{"handle when no name", &preview.Metadata{AuthorHandle: "jack", Description: "hi"}, "**jack** hi"},
		{"first line only", &preview.Metadata{Author: "A", Description: "\n first \nsecond"}, "**A** first"},
		{"description repeats title", &preview.Metadata{Title: "same", Description: "same"}, "**same**"},
		{"no author", &preview.Metadata{Description: "just text"}, "just text"},
		{"links don't embed", &preview.Metadata{Author: "A", Description: "see https://example.com/x"}, "**A** see <https://example.com/x>"},
	}
	for _, tt := range tests {
		if got := metadataText(tt.m); got != tt.want {
			t.Errorf("%s: metadataText = %q, want %q", tt.name, got, tt.want)
		}
	}

	long := metadataText(&preview.Metadata{Author: "A", Description: strings.Repeat("é", 1000)})
	if n := len([]rune(long)); n > maxDescriptionRunes+10 {
		t.Errorf("long description not truncated: %d runes", n)
	}
}
