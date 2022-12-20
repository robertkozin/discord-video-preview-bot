package main

import (
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/nathanielfernandes/cnvs/canvas"
	spreview "github.com/nathanielfernandes/cnvs/preview"
	"github.com/nathanielfernandes/cnvs/token"
)

var discordToken = mustGetEnvString("DISCORD_TOKEN")
var previewDir = mustGetEnvString("PREVIEW_DIR")
var tempDir = getEnvWithFallback("TEMP_DIR", "./tmp")
var previewBaseUrl = mustGetEnvString("PREVIEW_BASE_URL")

// TODO: Improve this to include short links
var previewMatch = regexp.MustCompile(`\S+(?:tiktok\.com|instagram\.com|twitter\.com|://t\.co|reddit\.com|redd\.it|clips\.twitch\.tv|youtube.com/shorts/)\S+`)
var spotifyMatch = regexp.MustCompile(`\S+open\.spotify\.com\/track\/([a-zA-Z0-9]+)\S+`)

var ytdlpPath = mustLookPath("yt-dlp")
var ffmpegPath = mustLookPath("ffmpeg")

var botID string

// make-shift hashset for saving preview urls
var cache = make(map[string]struct{})
var history = make(map[string]string)

func main() {
	// Ensure preview dir exists
	os.MkdirAll(previewDir, os.ModePerm)
	os.MkdirAll(tempDir, os.ModePerm)

	// Start cleaning task
	go func() {
		for range time.Tick(1 * time.Hour) {
			cleanHistory()
			if err := clean(previewDir, 10); err != nil {
				fmt.Println("err cleaning:", err)
			}
		}
	}()

	// start spotify runners
	token.StartAccessTokenReferesher()
	canvas.StartCanvasRunner()
	spreview.StartPreviewRunner()

	// Start discord bot
	dg, _ := discordgo.New("Bot " + discordToken)

	dg.Identify.Intents = discordgo.IntentsGuildMessages
	dg.Identify.LargeThreshold = 50

	dg.SyncEvents = false
	dg.StateEnabled = false

	dg.AddHandler(ready)
	dg.AddHandler(messageCreate)
	dg.AddHandler(messageDelete)

	err := dg.Open()
	if err != nil {
		fmt.Println("err starting discordgo:", err)
		return
	}

	fmt.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	dg.Close()
}

func ready(s *discordgo.Session, m *discordgo.Ready) {
	botID = m.User.ID
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == botID {
		return
	}

	output := ""
	if link := previewMatch.FindString(m.Content); link != "" {
		// TODO: Support multiple links?
		fmt.Println(link)

		s.ChannelTyping(m.ChannelID)

		output = preview(link)
	} else if spotifyMatch.MatchString(m.Content) {
		trackId := spotifyMatch.FindStringSubmatch(m.Content)[1]
		fmt.Println("spotify track id:", trackId)

		s.ChannelTyping(m.ChannelID)

		output = spotifyPreview(trackId)
	} else {
		return
	}

	if output == "" {
		return
	}

	data := discordgo.MessageSend{
		Content:         previewBaseUrl + output,
		Reference:       m.Reference(),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	}
	newMsg, err := s.ChannelMessageSendComplex(m.ChannelID, &data)
	if err != nil {
		fmt.Println("err sending message:", err)
		return
	}

	// add message to history
	history[m.ID] = newMsg.ID

	_, _ = s.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(m.ChannelID, m.ID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(m.ChannelID, ""))
}

func messageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if id, ok := history[m.ID]; ok {
		s.ChannelMessageDelete(m.ChannelID, id)
		delete(history, m.ID)
	}
}

func preview(url string) (path string) {
	hashUrl := fmt.Sprintf("%x", sha1.Sum([]byte(url)))[:7]
	outputFile := hashUrl + ".mp4"

	// if a preview was aldready generated, return it
	if _, ok := cache[outputFile]; ok {
		return outputFile
	}

	cmd := exec.Command(
		ytdlpPath,
		"--downloader", "ffmpeg", // Ffmpeg lets us limit video duration vs native downloader
		"--downloader-args", "ffmpeg:-to 60 -loglevel warning", // Limit to 60s
		"-S", "ext,+vcodec:avc", // Prefer mp4, H264
		// Assume that the places we're downloading from already optimize for the web (faststart + H264)
		"--no-mtime",    // Don't make output mtime the date of the video
		"--no-part",     // Seems like yt-dlp downloads videos as .part then renames. Don't think it's necessary in our case.
		"--no-playlist", // Don't download playlists, only single videos.
		"--playlist-items", "1",
		"--cookies", "./cookies.txt",
		"-o", outputFile,
		"-P", previewDir,
		url,
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return ""
	}

	// add filename to cache
	cache[outputFile] = struct{}{}

	return outputFile
}

// loop through all ids in the sent history and delete any id's older than a day
func cleanHistory() {
	yesterday := time.Now().Add(-24 * time.Hour)
	for id := range history {
		ts, err := discordgo.SnowflakeTimestamp(id)
		if err != nil || ts.Before(yesterday) {
			delete(history, id)
		}
	}
}

// If size of directory is greater than max then remove ~20% of the oldest files.
// Why leave 20% files? I don't know, 80/20 principal?
// Could be improved by using access time, but I'm not sure how I would get that information.
func clean(dir string, maxSizeGigabytes int) error {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return err
	}

	var maxSizeBytes int64 = int64(maxSizeGigabytes) * 1_000_000_000

	var totalDirSize int64
	for _, file := range files {
		totalDirSize += file.Size()
	}

	if totalDirSize <= maxSizeBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool { // Sort most recent first
		return files[i].ModTime().After(files[j].ModTime())
	})

	var runningDirSize int64 = 0
	var targetDirSize int64 = int64(float64(maxSizeBytes) * 0.80)
	for _, file := range files {
		runningDirSize += file.Size()
		if runningDirSize > targetDirSize {
			fn := file.Name()
			// delete preview from cache
			delete(cache, fn)
			if err = os.Remove(filepath.Join(dir, fn)); err != nil {
				return err
			}
		}
	}

	return nil
}

func mustGetEnvString(key string) (value string) {
	value, ok := os.LookupEnv(key)
	if !ok {
		panic("missing env var: " + key)
	}

	return value
}

func getEnvWithFallback(key string, fallback string) (value string) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}

	return value
}

func mustLookPath(file string) (path string) {
	path, err := exec.LookPath(file)
	if err != nil {
		panic("missing in path: " + file)
	}

	return path
}

func spotifyPreview(trackId string) (path string) {
	outputFile := trackId + ".mp4"
	// if a preview was aldready generated, return it
	if _, ok := cache[outputFile]; ok {
		return previewBaseUrl + outputFile
	}

	c, err := canvas.GetCanvas("spotify:track:" + trackId)
	if err != nil {
		fmt.Println("err getting canvas:", trackId)
		return
	}
	p, err := spreview.GetPreview(trackId)
	if (err != nil || p == spreview.PreviewResponse{}) {
		fmt.Println("err getting preview:", trackId)
		return
	}

	audiopreview_url := p.AudioURL
	canvas_url := p.CoverArtURL
	if c != nil {
		canvas_url = c.CanvasUrl
	}

	ext := "png"
	if strings.Contains(canvas_url, ".mp4") {
		ext = "mp4"
	} else if strings.Contains(canvas_url, ".jpg") {
		ext = "jpg"
	}

	canvas_path := filepath.Join(tempDir, fmt.Sprintf("%s-raw.%s", trackId, ext))
	defer os.Remove(canvas_path)
	if !download(canvas_url, canvas_path) {
		return
	}

	audiopreview_path := filepath.Join(tempDir, fmt.Sprintf("%s-raw.mp3", trackId))
	defer os.Remove(audiopreview_path)
	if !download(audiopreview_url, audiopreview_path) {
		return
	}

	outputPath := filepath.Join(previewDir, outputFile)

	var args []string
	if ext == "mp4" {
		args = []string{
			"-y", // overwrite output file
			"-stream_loop", "-1",
			"-i", canvas_path,
			"-i", audiopreview_path,
			"-map", "0:v:0",
			"-map", "1:a:0",
			"-t", "30",
			"-shortest",
			"-c:v", "copy",
			outputPath,
		}
	} else {
		// for png, jpg exts
		args = []string{
			"-y", // overwrite output file
			"-loop", "1",
			"-i", canvas_path,
			"-i", audiopreview_path,
			"-c:v", "libx264",
			"-tune:v", "stillimage",
			"-t", "30",
			"-shortest",
			"-filter:v", "fps=1",
			outputPath,
		}
	}

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return
	}

	cache[outputFile] = struct{}{}

	return outputFile
}

func download(url string, path string) bool {
	// Create the file
	out, err := os.Create(path)
	if err != nil {
		return false
	}

	defer out.Close()

	// Get the data
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.114 Safari/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}

	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return false
	}

	// Writer the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return false
	}

	fmt.Println("file downloaded:", path)
	return true
}
