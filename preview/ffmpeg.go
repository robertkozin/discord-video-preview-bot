package preview

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/tr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// muxTimeout bounds a single ffmpeg remux.
const muxTimeout = 60 * time.Second

const (
	// slideshowSecondsPerImage is how long each image of a slideshow shows.
	slideshowSecondsPerImage = 3
	// slideshowTimeout bounds encoding a slideshow (re-encodes, unlike a remux).
	slideshowTimeout = 120 * time.Second
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
func muxToMP4(ctx context.Context, inputs ...string) (_ string, err error) {
	ctx, span := tracer.Start(ctx, "mux_to_mp4")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.Int("mux.inputs", len(inputs)))

	// one trace spent 208s here; don't let a stalled remux hold a reply forever
	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()

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

	if st, statErr := os.Stat(path); statErr == nil {
		span.SetAttributes(attribute.Int64("mux.output_bytes", st.Size()))
	}

	return "file://" + path, nil
}

// webpToJPEG converts a still webp image to a jpeg, since the reuploader only
// stores mp4, jpeg and png.
func webpToJPEG(ctx context.Context, webp []byte) (_ []byte, err error) {
	ctx, span := tracer.Start(ctx, "webp_to_jpeg")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.Int("webp.bytes", len(webp)))

	ctx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg", "-loglevel", "error", "-f", "webp_pipe", "-i", "pipe:0",
		"-frames:v", "1", "-c:v", "mjpeg", "-q:v", "2", "-f", "image2pipe", "pipe:1")
	cmd.Stdin = bytes.NewReader(webp)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("converting webp with ffmpeg: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	if detectContentType(stdout.Bytes()) != "image/jpeg" {
		return nil, fmt.Errorf("converting webp with ffmpeg: output is not a jpeg")
	}
	span.SetAttributes(attribute.Int("jpeg.bytes", stdout.Len()))
	return stdout.Bytes(), nil
}

// slideshowToMP4 turns jpeg images (local paths) into one h264 video that shows
// each for slideshowSecondsPerImage, with audio (a local path, optional) under
// it, so a long slideshow fits Discord's gallery as one item. The canvas is
// portrait, landscape or square by the first image, at most 720 on the short
// side, with the others letterboxed. It returns a file:// url for the
// reuploader.
func slideshowToMP4(ctx context.Context, images []string, audio string) (_ string, err error) {
	ctx, span := tracer.Start(ctx, "slideshow_to_mp4")
	defer tr.End(span, &err)
	span.SetAttributes(attribute.Int("slideshow.images", len(images)), attribute.Bool("slideshow.audio", audio != ""))

	if len(images) == 0 {
		return "", fmt.Errorf("slideshow: no images")
	}
	ctx, cancel := context.WithTimeout(ctx, slideshowTimeout)
	defer cancel()

	width, height := 720, 1280
	if f, openErr := os.Open(images[0]); openErr == nil {
		cfg, _, decodeErr := image.DecodeConfig(f)
		_ = f.Close()
		switch {
		case decodeErr != nil || cfg.Height > cfg.Width:
		case cfg.Width > cfg.Height:
			width, height = 1280, 720
		default:
			width, height = 720, 720
		}
	}

	// the concat demuxer shows each file for its duration; the last file has to
	// be listed again, or ffmpeg drops its duration
	var list strings.Builder
	for _, img := range images {
		fmt.Fprintf(&list, "file '%s'\nduration %d\n", strings.ReplaceAll(img, "'", `'\''`), slideshowSecondsPerImage)
	}
	fmt.Fprintf(&list, "file '%s'\n", strings.ReplaceAll(images[len(images)-1], "'", `'\''`))
	listPath, err := newTempMediaFile("slideshow-*.txt")
	if err != nil {
		return "", err
	}
	defer os.Remove(listPath)
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing slideshow list: %w", err)
	}

	path, err := newTempMediaFile("slideshow-*.mp4")
	if err != nil {
		return "", err
	}

	total := len(images) * slideshowSecondsPerImage
	video := []string{
		"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1,format=yuv420p", width, height, width, height),
		// stills don't need 25 fps, and encoding the repeats is most of the work
		"-r", "5", "-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-crf", "28",
		"-t", fmt.Sprint(total), "-movflags", "+faststart", path,
	}
	run := func(audioArgs ...string) error {
		args := []string{"-loglevel", "error", "-y", "-f", "concat", "-safe", "0", "-i", listPath}
		if len(audioArgs) > 0 {
			args = append(args, "-i", audio, "-map", "0:v", "-map", "1:a:0")
			args = append(args, audioArgs...)
		}
		out, err := exec.CommandContext(ctx, "ffmpeg", append(args, video...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("making slideshow with ffmpeg: %w: %s", err, bytes.TrimSpace(out))
		}
		return nil
	}

	// TikTok's music is AAC in an mp4: copy it, since re-encoding minutes of
	// audio takes longer than everything else. Anything else (the muxer would
	// take even pcm, which browsers don't play in an mp4) is re-encoded.
	switch {
	case audio == "":
		err = run()
	case isMP4File(audio):
		if err = run("-c:a", "copy"); err != nil && ctx.Err() == nil {
			span.AddEvent("audio_copy_failed", trace.WithAttributes(attribute.String("error", err.Error())))
			err = run("-c:a", "aac", "-b:a", "128k")
		}
	default:
		err = run("-c:a", "aac", "-b:a", "128k")
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if st, statErr := os.Stat(path); statErr == nil {
		span.SetAttributes(attribute.Int64("slideshow.output_bytes", st.Size()))
	}
	return "file://" + path, nil
}

// isMP4File reports whether the file is an mp4/m4a container (an ftyp box first).
func isMP4File(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	return n == 12 && string(head[4:8]) == "ftyp"
}
