package preview

import (
	"context"
	"fmt"
	"net/url"
)

var _ Extractor = (*CobaltExtractor)(nil)

type CobaltExtractor struct {
	Endpoint string
	APIKey   string
}

func NewCobalt(config *url.URL) (*CobaltExtractor, error) {
	endpoint := *config
	query := config.Query()

	endpoint.RawQuery = ""

	endpoint.Scheme = "https"
	if query.Has("insecure") {
		endpoint.Scheme = "http"
	}

	apiKey := query.Get("key")

	return &CobaltExtractor{
		Endpoint: endpoint.String(),
		APIKey:   apiKey,
	}, nil
}

func (c *CobaltExtractor) String() string {
	return fmt.Sprintf("cobalt.tools at %s", c.Endpoint)
}

func (c *CobaltExtractor) IsSupported(url string) bool {
	return simpleURLMatch(url, []string{
		"instagram.com/reel/*",
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"vm.tiktok.com/*",
		"twitter.com/*/status/*",
		"t.co/*",
		"x.com/*/status/*",
		"bsky.app/profile/*/post/*",
		"twitch.tv/*/clip/*",
		"youtube.com/shorts/*",
		"reddit.com/r/*/comments/*",
		"old.reddit.com/r/*/comments/*",
		"redd.it/*",
		"v.redd.it/*",
	})
}

type CobaltRequest struct {
	Url string `json:"url"`
}

type CobaltError struct {
	Status string `json:"status"`
	Err    struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (ce CobaltError) Error() string {
	return "cobalt error: " + ce.Err.Code
}

type CobaltResponse struct {
	Status string         `json:"status"` // tunnel / local-processing / redirect / picker / error
	Url    string         `json:"url"`
	Picker []CobaltPicker `json:"picker"`
	Audio  any            `json:"audio"`
}

type CobaltPicker struct {
	Type string `json:"type"` // photo / video / gif
	Url  string `json:"url"`
}

func (c *CobaltExtractor) Extract(ctx context.Context, url string) ([]string, error) {
	var (
		req     = CobaltRequest{Url: url}
		headers []string
	)
	ctx, span := tracer.Start(ctx, "cobalt_extract")
	defer span.End()

	if c.APIKey != "" {
		headers = []string{"Authorization", "Api-Key " + c.APIKey}
	}

	_, value, err := JSONRequest[CobaltResponse, CobaltError](ctx, "POST", c.Endpoint, req, headers...)
	if err != nil {
		return nil, fmt.Errorf("making cobalt request: %w", err)
	}

	switch value.Status {
	case "redirect":
		return []string{value.Url}, nil
	case "tunnel":
		return []string{value.Url}, nil
	case "picker":
		urls := make([]string, len(value.Picker))
		for i, p := range value.Picker {
			urls[i] = p.Url
		}
		return urls, nil
	default:
		return nil, fmt.Errorf("unexpected cobalt response type: %s", value.Status)
	}
}
