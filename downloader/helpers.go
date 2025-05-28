package downloader

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tidwall/gjson"
	"github.com/tidwall/match"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func simpleURLMatch(url string, patterns []string) bool {
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "www.")
	for _, p := range patterns {
		if ok := match.Match(url, p); ok {
			return true
		}
	}
	return false
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
