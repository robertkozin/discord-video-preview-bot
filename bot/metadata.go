package bot

import (
	"strings"

	"github.com/robertkozin/discord-video-preview-bot/preview"
)

// maxDescriptionRunes keeps a long post from filling the reply.
const maxDescriptionRunes = 300

// metadataText is the reply's text line from what an extractor knew about the
// post: "**title or author** first line of the description". It returns "" when
// there is nothing to say, so the caller can fall back to Discord's embed.
func metadataText(m *preview.Metadata) string {
	if m.Empty() {
		return ""
	}

	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = strings.TrimSpace(m.Author)
	}
	if title == "" {
		title = strings.TrimSpace(m.AuthorHandle)
	}

	desc := firstLine(m.Description)
	if desc == title {
		desc = ""
	}
	desc = preventURLEmbeds(truncateRunes(desc, maxDescriptionRunes))

	switch {
	case title != "" && desc != "":
		return "**" + title + "** " + desc
	case title != "":
		return "**" + title + "**"
	default:
		return desc
	}
}

// firstLine is the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
