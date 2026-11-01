package preview

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
)

var _ Extractor = (*YtDlpExtractor)(nil)

// YtDlpExtractor downloads with the yt-dlp cli (which uses ffmpeg to merge
// formats) into tempMediaDir and returns a file:// url.
type YtDlpExtractor struct {
	// Proxy is passed to --proxy, e.g. http://user:pass@host:port
	Proxy string
}

const ytDlpTimeout = 3 * time.Minute

func NewYtDlp(config *url.URL) (*YtDlpExtractor, error) {
	proxy := config.Query().Get("proxy")
	if proxy != "" {
		if _, err := url.Parse(proxy); err != nil {
			return nil, fmt.Errorf("parsing ytdlp proxy: %w", err)
		}
	}
	return &YtDlpExtractor{Proxy: proxy}, nil
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

func (ex *YtDlpExtractor) Extract(ctx context.Context, mediaURL string) ([]string, error) {
	var err error
	ctx, span := tracer.Start(ctx, "ytdlp_extract")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.Bool("proxy", ex.Proxy != ""))

	ctx, cancel := context.WithTimeout(ctx, ytDlpTimeout)
	defer cancel()

	// reserve a unique name, then let yt-dlp pick the extension
	reserved, err := newTempMediaFile("ytdlp-*.mp4")
	if err != nil {
		return nil, err
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
		"--print", "after_move:filepath",
	}
	if ex.Proxy != "" {
		args = append(args, "--proxy", ex.Proxy)
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
			return nil, err
		}
		err = fmt.Errorf("running yt-dlp: %w: %s", err, msg)
		return nil, err
	}

	path := lastLine(stdout.String())
	if path == "" {
		// e.g. skipped because of --max-filesize
		removeMatching(base + "*")
		err = fmt.Errorf("yt-dlp produced no file: %s", lastLine(stderr.String()))
		return nil, err
	}
	if !isTempMediaFile(path) {
		err = fmt.Errorf("yt-dlp wrote outside the temp media dir: %s", path)
		return nil, err
	}

	return []string{"file://" + path}, nil
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
