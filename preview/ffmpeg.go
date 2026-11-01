package preview

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// tempMediaDir holds files that extractors produce locally. They are handed to
// the reuploader as file:// urls, which deletes them once read.
var tempMediaDir = filepath.Join(os.TempDir(), "discord-video-preview")

// newTempMediaFile reserves a path in tempMediaDir, e.g. pattern "mux-*.mp4".
func newTempMediaFile(pattern string) (string, error) {
	if err := os.MkdirAll(tempMediaDir, 0o755); err != nil {
		return "", fmt.Errorf("creating temp media dir: %w", err)
	}
	f, err := os.CreateTemp(tempMediaDir, pattern)
	if err != nil {
		return "", fmt.Errorf("creating temp media file: %w", err)
	}
	_ = f.Close()
	return f.Name(), nil
}

func isTempMediaFile(path string) bool {
	rel, err := filepath.Rel(tempMediaDir, filepath.Clean(path))
	return err == nil && !strings.HasPrefix(rel, "..") && rel != "."
}

// muxToMP4 merges every stream of the inputs (urls or local paths), e.g. a
// video-only and an audio-only track, into one mp4 without re-encoding.
// It returns a file:// url for the reuploader.
func muxToMP4(ctx context.Context, inputs ...string) (string, error) {
	ctx, span := tracer.Start(ctx, "mux_to_mp4")
	defer span.End()

	if len(inputs) == 0 {
		return "", fmt.Errorf("mux: no inputs")
	}

	path, err := newTempMediaFile("mux-*.mp4")
	if err != nil {
		return "", err
	}

	args := []string{"-loglevel", "error", "-y"}
	for _, in := range inputs {
		args = append(args, "-i", in)
	}
	for i := range inputs {
		args = append(args, "-map", fmt.Sprintf("%d", i))
	}
	args = append(args, "-c", "copy", "-movflags", "+faststart", path)

	out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("muxing with ffmpeg: %w: %s", err, bytes.TrimSpace(out))
	}

	return "file://" + path, nil
}
