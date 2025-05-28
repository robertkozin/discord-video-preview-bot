package downloader

import (
	"errors"
	"fmt"
	"net/http"
)

type CobaltDownloader struct {
	Dir      string
	Endpoint string
	APIKey   string
}

func (c *CobaltDownloader) String() string {
	return fmt.Sprintf("cobalt (%s)", c.Endpoint)
}

func (c *CobaltDownloader) MatchURL(url string) bool {
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
	})
}

type CobaltRequest struct {
	Url string `json:"url"`
}

type CobaltResponse struct {
	Status string `json:"status"` // error / redirect / stream / success / rate-limit / picker
	Error  struct {
		Code    string `json:"code"`
		Context struct {
			Service string `json:"service"`
			Limit   int    `json:"limit"`
		} `json:"context"`
	} `json:"error"`
	OriginalUrl string
	Url         string         `json:"url"`
	PickerType  string         `json:"pickerType"` // various / images
	Picker      []CobaltPicker `json:"picker"`
	Audio       any            `json:"audio"`
}

type CobaltPicker struct {
	Type  string `json:"type"` // video, used only if pickerTypeis various
	Url   string `json:"url"`
	Thumb string `json:"thumb"`
}

func (c *CobaltDownloader) Download(url string) (string, error) {
	var creq = CobaltRequest{Url: url}
	var res CobaltResponse

	req, _ := http.NewRequest(http.MethodPost, c.Endpoint, jsonMarshalReader(creq))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Api-Key "+c.APIKey)
	}

	err := doJson3(req, &res)
	if err != nil {
		return "", fmt.Errorf("doJson3: %v", err)
	}

	switch res.Status {
	case "redirect":
		return downloadMedia(res.Url, c.Dir, sha12(url))
	case "tunnel":
		return downloadMedia(res.Url, c.Dir, sha12(url))
	//case "picker":
	//	return downloadPicker(&res, filename)
	case "error":
		return "", errors.New(res.Error.Code)
	default:
		return "", fmt.Errorf("status not supported %v", res.Status)
	}
}
