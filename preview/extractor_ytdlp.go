package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

var _ Extractor = (*YtDlpExtractor)(nil)

// YtDlpExtractor downloads with the yt-dlp cli (which uses ffmpeg to merge
// formats) into tempMediaDir and returns a file:// url.
type YtDlpExtractor struct {
	// Proxy is passed to --proxy, e.g. http://user:pass@host:port
	Proxy string
	// POTProvider is the base url of a bgutil-ytdlp-pot-provider server, which
	// generates YouTube PO tokens for the bgutil plugin in the image.
	POTProvider string
	// YouTubeClients is yt-dlp's youtube player_client list, e.g.
	// "tv_samsung,mweb" to stay independent of the youtube extractor's
	// visionos client. Empty is yt-dlp's default.
	YouTubeClients string
}

const ytDlpTimeout = 3 * time.Minute

func NewYtDlp(config *url.URL) (*YtDlpExtractor, error) {
	proxy := config.Query().Get("proxy")
	if proxy != "" {
		if _, err := url.Parse(proxy); err != nil {
			return nil, fmt.Errorf("parsing ytdlp proxy: %w", err)
		}
	}
	pot := config.Query().Get("pot")
	if pot != "" {
		if _, err := url.Parse(pot); err != nil {
			return nil, fmt.Errorf("parsing ytdlp pot provider url: %w", err)
		}
	}
	return &YtDlpExtractor{Proxy: proxy, POTProvider: pot, YouTubeClients: config.Query().Get("youtube_clients")}, nil
}

func (ex *YtDlpExtractor) String() string {
	return "yt-dlp"
}

func (ex *YtDlpExtractor) IsSupported(mediaURL string) bool {
	return simpleURLMatch(mediaURL, []string{
		"youtube.com/shorts/*",
		"instagram.com/reel/*",
		"instagram.com/reels/*",
		"instagram.com/p/*",
		"tiktok.com/t/*",
		"tiktok.com/@*/video/*",
		"*.tiktok.com/*",
		"x.com/*/status/*",
		"twitter.com/*/status/*",
		"reddit.com/r/*/comments/*",
		"old.reddit.com/r/*/comments/*",
		"bsky.app/profile/*/post/*",
		"twitch.tv/*/clip/*",
		"clips.twitch.tv/*",
	})
}

var _ MediaExtractor = (*YtDlpExtractor)(nil)

func (ex *YtDlpExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	media, err := ex.ExtractMedia(ctx, mediaURL)
	return media.URLs, err
}

// ytDlpInfoTemplate prints the post details yt-dlp already has as one JSON
// line (fields that are missing are left out).
const ytDlpInfoTemplate = "%(.{title,description,uploader,channel,uploader_id,like_count,view_count,comment_count,repost_count,timestamp})j"

type ytDlpInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Uploader    string `json:"uploader"`
	Channel     string `json:"channel"`
	UploaderID  string `json:"uploader_id"`
	LikeCount   int64  `json:"like_count"`
	ViewCount   int64  `json:"view_count"`
	Comments    int64  `json:"comment_count"`
	Reposts     int64  `json:"repost_count"`
	Timestamp   int64  `json:"timestamp"`
}

func (i ytDlpInfo) metadata() *Metadata {
	m := &Metadata{
		Title:        i.Title,
		Description:  i.Description,
		Author:       i.Uploader,
		AuthorHandle: i.UploaderID,
		Likes:        i.LikeCount,
		Views:        i.ViewCount,
		Comments:     i.Comments,
		Shares:       i.Reposts,
		Source:       "ytdlp",
	}
	if m.Author == "" {
		m.Author = i.Channel
	}
	if strings.TrimSpace(m.Title) == strings.TrimSpace(m.Description) {
		m.Description = "" // many sites repeat the caption as both
	}
	if i.Timestamp > 0 {
		m.PostedAt = time.Unix(i.Timestamp, 0).UTC()
	}
	return m
}

// parseYtDlpInfo finds the info line in yt-dlp's stdout; the file path is the last line.
func parseYtDlpInfo(stdout string) *Metadata {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var info ytDlpInfo
		if json.Unmarshal([]byte(line), &info) == nil {
			return info.metadata()
		}
	}
	return nil
}

func (ex *YtDlpExtractor) ExtractMedia(ctx context.Context, mediaURL string) (_ Media, err error) {
	ctx, span := tracer.Start(ctx, "ytdlp_extract")
	defer EndSpan(span, &err)
	span.SetAttributes(attribute.Bool("proxy", ex.Proxy != ""), attribute.Bool("pot_provider", ex.POTProvider != ""))

	ctx, cancel := context.WithTimeout(ctx, ytDlpTimeout)
	defer cancel()

	// reserve a unique name, then let yt-dlp pick the extension
	reserved, err := newTempMediaFile("ytdlp-*.mp4")
	if err != nil {
		return Media{}, err
	}
	_ = os.Remove(reserved)
	base := strings.TrimSuffix(reserved, ".mp4")

	args := []string{
		"--no-update", "--no-playlist", "--no-progress",
		"--max-filesize", fmt.Sprintf("%d", MaxMediaSize),
		// prefer something discord plays inline without being huge
		"-S", "vcodec:h264,res:720,acodec:aac",
		"--merge-output-format", "mp4",
		"--remux-video", "mp4",
		"-o", base + ".%(ext)s",
		// the info line comes first and the file path last
		"--print", ytDlpInfoTemplate,
		"--print", "after_move:filepath",
	}
	if ex.Proxy != "" {
		args = append(args, "--proxy", ex.Proxy)
	}
	if ex.POTProvider != "" {
		args = append(args, "--extractor-args", "youtubepot-bgutilhttp:base_url="+ex.POTProvider)
	}
	if ex.YouTubeClients != "" {
		args = append(args, "--extractor-args", "youtube:player_client="+ex.YouTubeClients)
	}
	args = append(args, "--", mediaURL)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// yt-dlp spawns ffmpeg; kill the whole process group on timeout, and don't
	// wait forever on pipes held open by children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	if err = cmd.Run(); err != nil {
		removeMatching(base + "*")
		msg := lastLine(stderr.String())
		if ytDlpNoVideo(msg) {
			err = fmt.Errorf("yt-dlp: %s: %w", msg, ErrNoVideo)
			return Media{}, err
		}
		err = fmt.Errorf("running yt-dlp: %w: %s", err, msg)
		return Media{}, err
	}

	path := lastLine(stdout.String())
	if path == "" {
		// e.g. skipped because of --max-filesize
		removeMatching(base + "*")
		err = fmt.Errorf("yt-dlp produced no file: %s", lastLine(stderr.String()))
		return Media{}, err
	}
	if !isTempMediaFile(path) {
		err = fmt.Errorf("yt-dlp wrote outside the temp media dir: %s", path)
		return Media{}, err
	}

	return Media{URLs: []string{"file://" + path}, Metadata: parseYtDlpInfo(stdout.String())}, nil
}

// ytDlpNoVideo reports whether yt-dlp loaded the post and found no video in it
// (text or photo-only posts). Only messages that can't come from a block or a
// login wall count, e.g. not instagram's "empty media response".
func ytDlpNoVideo(stderr string) bool {
	return (strings.Contains(stderr, "[twitter]") && strings.Contains(stderr, "No video could be found in this tweet")) ||
		(strings.Contains(stderr, "[Bluesky]") && strings.Contains(stderr, "No video could be found in this post")) ||
		(strings.Contains(stderr, "[Instagram]") && strings.Contains(stderr, "There is no video in this post"))
}

func removeMatching(pattern string) {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
