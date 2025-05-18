package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tidwall/gjson"
	"io"
	"log/slog"
	"mime"
	"net/http"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"strings"
)

type VideoDownloader interface {
	MatchURL(url string) (ok bool)
	Download(url string) (file string, err error)
}

type DownloaderRegistry struct {
	downloaders []VideoDownloader
}

func (d *DownloaderRegistry) Add(v VideoDownloader) {
	d.downloaders = append(d.downloaders, v)
}

func (d *DownloaderRegistry) HasMatch(url string) (ok bool) {
	for _, dl := range d.downloaders {
		if dl.MatchURL(url) {
			return true
		}
	}
	return false
}

func (d *DownloaderRegistry) Download(url string) (file string, err error) {
	for _, dl := range d.downloaders {
		if dl.MatchURL(url) {
			file, err = dl.Download(url)
			if err != nil {
				slog.Error("error downloading", "downloader", dl, "url", url, "err", err)
				continue
			}
			return file, nil
		}
	}
	return "", nil
}

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

func doJson3(req *http.Request, dest any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http.DefaultClient.Do: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("io.ReadAll: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not OK: %s", resp.Status)
	}
	err = json.Unmarshal(b, dest)
	if err != nil {
		return fmt.Errorf("json.Unmarshal: %v", err)
	}
	return nil
}

func doJson2(req *http.Request, jsonPath string) (gjson.Result, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("DefaultClient.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return gjson.Result{}, fmt.Errorf("not OK: %d %s", resp.StatusCode, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return gjson.Result{}, fmt.Errorf("io.ReadAll: %v", err)
	}
	if jsonPath != "" {
		return gjson.GetBytes(b, jsonPath), nil
	}
	return gjson.ParseBytes(b), nil
}

func downloadMedia(url string, dir string, name string) (string, error) {
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", fmt.Errorf("failed to create directory: %v", err)
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("not OK: %d %s", resp.StatusCode, resp.Status)
	}

	// todo max size check
	// todo only mp4 check

	ext := getResponseExtension(resp)

	path := filepath.Join(dir, name+ext)

	out, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return "", err
	}
	defer out.Close()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return "", err
	}

	return path, nil

}

func getResponseExtension(resp *http.Response) string {
	// Check Content-Disposition first
	{
		if cd := resp.Header.Get("Content-Disposition"); cd != "" {
			_, params, _ := mime.ParseMediaType(cd)
			if filename, ok := params["filename"]; ok {
				ext := filepath.Ext(filename)
				if ext != "" {
					return strings.ToLower(ext)
				}
			}
		}
	}

	// Check Content-Type
	{
		mediatype, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		exts, err := mime.ExtensionsByType(mediatype)
		if err == nil && len(exts) > 0 {
			return strings.ToLower(exts[0])
		}
	}

	// Check extension in url
	{
		ext := filepath.Ext(resp.Request.URL.Path)
		if ext != "" {
			return strings.ToLower(ext)
		}
	}

	return ""
}

func sha12(s string) string {
	hash := sha256.Sum256([]byte(s))
	shortHash := hex.EncodeToString(hash[:])[:12] // 12 chars
	return shortHash
}

func jsonMarshalReader(v any) *bytes.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("jsonMarshalReader: %v", err))
	}
	return bytes.NewReader(b)
}
