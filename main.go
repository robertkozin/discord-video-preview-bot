package main

import (
	"crypto/sha1"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

var discordToken = mustGetEnvString("DISCORD_TOKEN")
var previewDir = mustGetEnvString("PREVIEW_DIR")
var previewBaseUrl = mustGetEnvString("PREVIEW_BASE_URL")

// TODO: Improve this to include short links
var previewMatch = regexp.MustCompile(`\S+(?:tiktok\.com|instagram\.com|twitter\.com|://t\.co|reddit\.com|redd\.it|clips\.twitch\.tv)\S+`)
var ytdlpPath = mustLookPath("yt-dlp")

var botID string

// make-shift hashset for saving preview urls
var cache = map[string]bool{}

func main() {
	// Ensure preview dir exists
	os.MkdirAll(previewDir, os.ModePerm)

	// Start cleaning task
	go func() {
		for range time.Tick(1 * time.Hour) {
			if err := clean(previewDir, 10); err != nil {
				fmt.Println("err cleaning:", err)
			}
		}
	}()

	// Start discord bot
	dg, _ := discordgo.New("Bot " + discordToken)

	dg.Identify.Intents = discordgo.IntentsGuildMessages
	dg.Identify.LargeThreshold = 50
	dg.Identify.GuildSubscriptions = false

	dg.SyncEvents = false
	dg.StateEnabled = false

	dg.AddHandler(ready)
	dg.AddHandler(messageCreate)

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

	link := previewMatch.FindString(m.Content)
	if link == "" {
		return
	}

	// TODO: Support multiple links?
	fmt.Println(link)

	output := preview(link)
	if output == "" {
		return
	}

	if _, err := s.ChannelMessageSend(m.ChannelID, previewBaseUrl+output); err != nil {
		fmt.Println("err sending message:", err)
		return
	}

	_, _ = s.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(m.ChannelID, m.ID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(m.ChannelID, ""))
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
	cache[outputFile] = true

	return outputFile
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

func mustLookPath(file string) (path string) {
	path, err := exec.LookPath(file)
	if err != nil {
		panic("missing in path: " + file)
	}

	return path
}
