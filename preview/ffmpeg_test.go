package preview

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testJPEG is a solid jpeg of the given size.
func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 2), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestSlideshowToMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	var images []string
	for i, size := range [][2]int{{90, 160}, {160, 90}, {100, 100}} { // later ones get letterboxed
		path := filepath.Join(dir, "img"+strconv.Itoa(i)+".jpg")
		if err := os.WriteFile(path, testJPEG(size[0], size[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		images = append(images, path)
	}
	audio := filepath.Join(dir, "music.m4a")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "sine=duration=2", "-c:a", "aac", audio).CombinedOutput(); err != nil {
		t.Fatalf("making test audio: %v: %s", err, out)
	}

	// pcm can't be copied into an mp4 as is, so it takes the re-encode fallback
	wav := filepath.Join(dir, "music.wav")
	if out, err := exec.Command("ffmpeg", "-loglevel", "error", "-f", "lavfi", "-i", "sine=duration=2", "-c:a", "pcm_s16le", wav).CombinedOutput(); err != nil {
		t.Fatalf("making test wav: %v: %s", err, out)
	}

	for name, music := range map[string]string{"silent": "", "aac music (copied)": audio, "wav music (re-encoded)": wav} {
		t.Run(name, func(t *testing.T) {
			u, err := slideshowToMP4(context.Background(), images, music)
			if err != nil {
				t.Fatal(err)
			}
			path := strings.TrimPrefix(u, "file://")
			defer os.Remove(path)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if detectContentType(body) != "video/mp4" {
				t.Fatalf("content type = %s", detectContentType(body))
			}
			if _, err := exec.LookPath("ffprobe"); err != nil {
				return
			}
			out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name,width,height", "-of", "default=nw=1", path).CombinedOutput()
			if err != nil {
				t.Fatalf("ffprobe: %v: %s", err, out)
			}
			info := string(out)
			// 3 images × slideshowSecondsPerImage, portrait canvas from the first image
			want := float64(3 * slideshowSecondsPerImage)
			var got float64
			for _, line := range strings.Split(info, "\n") {
				if v, ok := strings.CutPrefix(line, "duration="); ok {
					got, _ = strconv.ParseFloat(v, 64)
				}
			}
			if got < want-0.5 || got > want+0.5 {
				t.Errorf("duration = %v, want ≈%v\n%s", got, want, info)
			}
			if !strings.Contains(info, "width=720") || !strings.Contains(info, "height=1280") || !strings.Contains(info, "codec_name=h264") {
				t.Errorf("unexpected streams:\n%s", info)
			}
			if hasAudio := strings.Contains(info, "codec_name=aac"); hasAudio != (music != "") {
				t.Errorf("audio stream = %v, want %v", hasAudio, music != "")
			}
		})
	}
}
