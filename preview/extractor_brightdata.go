package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
)

var _ Extractor = (*BrightDataExtractor)(nil)

// BrightDataExtractor uses the Bright Data dataset (Web Scraper) API. It is slow
// (X is a few seconds, Instagram ~30-75s) so it should be last.
type BrightDataExtractor struct {
	APIKey string
}

type brightDataset struct {
	id       string
	patterns []string
	// media pulls downloadable urls out of a record
	media func(rec brightDataRecord) ([]string, error)
}

var brightDatasets = []brightDataset{
	{
		id: "gd_lyclm20il4r5helnj", // Instagram - Reels
		patterns: []string{
			"instagram.com/reel/*",
			"instagram.com/reels/*",
		},
		media: func(rec brightDataRecord) ([]string, error) {
			var videoURL string
			if err := json.Unmarshal(rec["video_url"], &videoURL); err != nil || videoURL == "" {
				return nil, errors.New("record has no video_url")
			}
			return []string{videoURL}, nil
		},
	},
	{
		id: "gd_lwxkxvnf1cynvib9co", // X (formerly Twitter) - Posts
		patterns: []string{
			"x.com/*/status/*",
			"twitter.com/*/status/*",
		},
		media: func(rec brightDataRecord) ([]string, error) {
			var videos []struct {
				VideoURL string `json:"video_url"`
			}
			_ = json.Unmarshal(rec["videos"], &videos)
			urls := make([]string, 0, len(videos))
			for _, v := range videos {
				if v.VideoURL != "" {
					urls = append(urls, v.VideoURL)
				}
			}
			if len(urls) == 0 {
				return nil, errors.New("record has no videos")
			}
			return urls, nil
		},
	},
}

// brightDataRecord is decoded lazily since every dataset has its own schema.
type brightDataRecord map[string]json.RawMessage

// str returns the first of keys that holds a non-empty string.
func (rec brightDataRecord) str(keys ...string) string {
	for _, k := range keys {
		var s string
		if json.Unmarshal(rec[k], &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// num returns the first of keys that holds a number (or a numeric string).
func (rec brightDataRecord) num(keys ...string) int64 {
	for _, k := range keys {
		var f float64
		if json.Unmarshal(rec[k], &f) == nil && f > 0 {
			return int64(f)
		}
		var s string
		if json.Unmarshal(rec[k], &s) == nil {
			if n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// metadata reads the post details from a record. The datasets name their
// fields differently, so it tries the likely keys of each; fields it can't
// find stay empty.
func (rec brightDataRecord) metadata() *Metadata {
	m := &Metadata{
		Description:  rec.str("description", "caption", "text"),
		Author:       rec.str("name", "full_name", "profile_name"),
		AuthorHandle: rec.str("user_posted", "username", "user_name"),
		Likes:        rec.num("likes", "like_count"),
		Views:        rec.num("views", "video_play_count", "video_view_count", "play_count"),
		Comments:     rec.num("num_comments", "comments", "replies", "comment_count"),
		Shares:       rec.num("reposts", "shares"),
		Source:       "brightdata",
	}
	if t, err := time.Parse(time.RFC3339, rec.str("date_posted", "created_at")); err == nil {
		m.PostedAt = t.UTC()
	}
	return m
}

func (rec brightDataRecord) err() error {
	var code, msg string
	_ = json.Unmarshal(rec["error_code"], &code)
	_ = json.Unmarshal(rec["error"], &msg)
	if code == "" && msg == "" {
		return nil
	}
	return fmt.Errorf("bright data: %s (%s)", msg, code)
}

const (
	brightDataAPI          = "https://api.brightdata.com/datasets/v3"
	brightDataPollInterval = 2 * time.Second
	brightDataMaxWait      = 5 * time.Minute
)

// the sync scrape endpoint holds the request open for up to ~60s
var brightDataClient = &http.Client{
	Transport: http.DefaultTransport,
	Timeout:   90 * time.Second,
}

func NewBrightData(config *url.URL) (*BrightDataExtractor, error) {
	apiKey := config.Query().Get("key")
	if apiKey == "" {
		return nil, errors.New("brightdata extractor needs ?key=<api key>")
	}
	return &BrightDataExtractor{APIKey: apiKey}, nil
}

func (ex *BrightDataExtractor) String() string {
	return "bright data datasets api"
}

func (ex *BrightDataExtractor) dataset(mediaURL string) (brightDataset, bool) {
	for _, ds := range brightDatasets {
		if simpleURLMatch(mediaURL, ds.patterns) {
			return ds, true
		}
	}
	return brightDataset{}, false
}

func (ex *BrightDataExtractor) IsSupported(mediaURL string) bool {
	_, ok := ex.dataset(mediaURL)
	return ok
}

var _ MediaExtractor = (*BrightDataExtractor)(nil)

func (ex *BrightDataExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

func (ex *BrightDataExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "brightdata_extract")
	defer tr.End(span, &err)

	ds, ok := ex.dataset(mediaURL)
	if !ok {
		err = fmt.Errorf("no bright data dataset for %s", mediaURL)
		return Media{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, brightDataMaxWait)
	defer cancel()

	records, err := ex.scrape(ctx, ds.id, mediaURL)
	if err != nil {
		return Media{}, err
	}
	if len(records) == 0 {
		err = errors.New("bright data returned no records")
		return Media{}, err
	}
	if err = records[0].err(); err != nil {
		return Media{}, err
	}

	urls, err := ds.media(records[0])
	if err != nil {
		return Media{}, err
	}
	return Media{URLs: urls, Metadata: records[0].metadata()}, nil
}

// scrape uses the sync endpoint, which falls back to a snapshot that has to be
// polled when collection takes longer than ~60s.
func (ex *BrightDataExtractor) scrape(ctx context.Context, datasetID, mediaURL string) ([]brightDataRecord, error) {
	q := url.Values{
		"dataset_id":     {datasetID},
		"notify":         {"false"},
		"include_errors": {"true"},
		"format":         {"json"},
	}
	body := map[string]any{"input": []map[string]string{{"url": mediaURL}}}

	status, respBody, err := ex.request(ctx, "POST", brightDataAPI+"/scrape?"+q.Encode(), body)
	if err != nil {
		return nil, err
	}

	switch status {
	case http.StatusOK:
		var records []brightDataRecord
		if err := json.Unmarshal(respBody, &records); err != nil {
			return nil, fmt.Errorf("parsing bright data records: %w", err)
		}
		return records, nil
	case http.StatusAccepted:
		var pending struct {
			SnapshotID string `json:"snapshot_id"`
		}
		if err := json.Unmarshal(respBody, &pending); err != nil || pending.SnapshotID == "" {
			return nil, fmt.Errorf("parsing bright data snapshot id: %s", respBody)
		}
		return ex.waitForSnapshot(ctx, pending.SnapshotID)
	default:
		return nil, fmt.Errorf("bright data scrape: %d: %s", status, respBody)
	}
}

func (ex *BrightDataExtractor) waitForSnapshot(ctx context.Context, snapshotID string) ([]brightDataRecord, error) {
	ticker := time.NewTicker(brightDataPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for bright data snapshot %s: %w", snapshotID, ctx.Err())
		case <-ticker.C:
		}

		status, respBody, err := ex.request(ctx, "GET", brightDataAPI+"/progress/"+snapshotID, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("bright data progress: %d: %s", status, respBody)
		}
		var progress struct {
			Status string `json:"status"` // starting / running / ready / failed
		}
		if err := json.Unmarshal(respBody, &progress); err != nil {
			return nil, fmt.Errorf("parsing bright data progress: %w", err)
		}

		switch progress.Status {
		case "ready":
			status, respBody, err := ex.request(ctx, "GET", brightDataAPI+"/snapshot/"+snapshotID+"?format=json", nil)
			if err != nil {
				return nil, err
			}
			if status != http.StatusOK {
				return nil, fmt.Errorf("bright data snapshot: %d: %s", status, respBody)
			}
			var records []brightDataRecord
			if err := json.Unmarshal(respBody, &records); err != nil {
				return nil, fmt.Errorf("parsing bright data snapshot: %w", err)
			}
			return records, nil
		case "failed":
			return nil, fmt.Errorf("bright data snapshot %s failed", snapshotID)
		}
	}
}

func (ex *BrightDataExtractor) request(ctx context.Context, method, endpoint string, body any) (int, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+ex.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpDoWith(brightDataClient, req)
	if err != nil {
		return 0, nil, fmt.Errorf("sending bright data request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading bright data response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
