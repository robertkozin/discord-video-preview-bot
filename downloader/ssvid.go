package downloader

import (
	"fmt"
	"net/http"
	urlpkg "net/url"
	"strings"
)

type SsvidDownloader struct {
	Dir string
}

func (s *SsvidDownloader) MatchURL(url string) (ok bool) {
	return simpleURLMatch(url, []string{
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"vm.tiktok.com/*",
		"instagram.com/reel/*",
	})
}

type ssvidResponse struct {
	Status string `json:"status"`
	Mess   string `json:"mess"`
	P      string `json:"p"`
	Data   struct {
		Page      string `json:"page"`
		Extractor string `json:"extractor"`
		Status    string `json:"status"`
		Keyword   string `json:"keyword"`
		Title     string `json:"title"`
		Thumbnail string `json:"thumbnail"`
		Pid       string `json:"pid"`
		Links     struct {
			Video []struct {
				QText string `json:"q_text"`
				Size  string `json:"size"`
				URL   string `json:"url"`
			} `json:"video"`
		} `json:"links"`
		Author struct {
			Username string `json:"username"`
			FullName string `json:"full_name"`
			Avatar   string `json:"avatar"`
		} `json:"author"`
	} `json:"data"`
}

func (s *SsvidDownloader) Download(url string) (file string, err error) {
	data := urlpkg.Values{
		"query": {url},
		"vt":    {"home"},
	}
	req, _ := http.NewRequest("POST", "https://www.ssvid.net/api/ajax/search", strings.NewReader(data.Encode()))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.3.1 Safari/605.1.15")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Host", "www.ssvid.net")
	req.Header.Set("Origin", "https://www.ssvid.net")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")

	var ssvidResp ssvidResponse
	err = doJson3(req, &ssvidResp)
	if err != nil {
		return "", fmt.Errorf("doJson3: %v", err)
	}

	if len(ssvidResp.Data.Links.Video) == 0 {
		return "", fmt.Errorf("no videos in response")
	}

	return downloadMedia(ssvidResp.Data.Links.Video[0].URL, s.Dir, sha12(url))
}
