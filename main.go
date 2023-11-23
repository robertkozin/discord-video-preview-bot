package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log/slog"
	"mime"
	"net/http"
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
var previewMatch = regexp.MustCompile(`\S+(?:tiktok\.com|instagram\.com|twitter\.com|://t\.co|reddit\.com|redd\.it|clips\.twitch\.tv|youtube.com/shorts/|://x.com)\S+`)

var botID string

var history = make(map[string]string)

var cobaltEndpoint = "https://co.wuk.sh/api/json"

type CobaltRequest struct {
	Url             string `json:"url"`
	IsNoTTWatermark bool   `json:"isNoTTWatermark"`
}

type CobaltResponse struct {
	Status      string `json:"status"` // error / redirect / stream / success / rate-limit / picker
	Text        string `json:"text"`
	OriginalUrl string
	Url         string         `json:"url"`
	PickerType  string         `json:"pickerType"` // various / images
	Picker      []CobaltPicker `json:"picker"`
	Audio       string         `json:"audio"`
}

type CobaltPicker struct {
	Type  string `json:"type"` // video, used only if pickerTypeis various
	Url   string `json:"url"`
	Thumb string `json:"thumb"`
}

func main() {
	// Ensure preview dir exists
	os.MkdirAll(previewDir, os.ModePerm)

	// Start cleaning task
	go func() {
		for range time.Tick(1 * time.Hour) {
			cleanHistory()
			if err := clean(previewDir, 10); err != nil {
				fmt.Println("err cleaning:", err)
			}
		}
	}()

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

func getFilename(filename string, contentType string) (string, error) {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("error parsing mimetype: %v: %v", contentType, err)
	}
	ext, ok := mimeExtension[mediatype]
	if !ok {
		return "", fmt.Errorf("mimetype not supported: %v: %v", mediatype, err)
	}

	return filepath.Join(previewDir, filename+ext), nil
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == botID {
		return
	}

	link := previewMatch.FindString(m.Content)
	if link == "" {
		return
	}

	_ = s.ChannelTyping(m.ChannelID)

	path, err := preview(link)
	if err != nil {
		slog.Error("preview error", "msg", m.Content, "link", link, "err", err)
		return
	}
	base := filepath.Base(path)
	reply := previewBaseUrl + base

	_, _ = s.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(m.ChannelID, m.ID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(m.ChannelID, ""))

	newMsg, err := s.ChannelMessageSend(m.ChannelID, reply)
	if err != nil {
		slog.Error("err sending message", "err", err)
		return
	}

	// add message to history
	history[m.ID] = newMsg.ID
}

func messageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if id, ok := history[m.ID]; ok {
		s.ChannelMessageDelete(m.ChannelID, id)
		delete(history, m.ID)
	}
}

func preview(url string) (path string, err error) {
	filename := fmt.Sprintf("%x", sha1.Sum([]byte(url)))[:7] // First 7 chars of sha1 hash of url

	var req = CobaltRequest{
		Url: url,
	}
	var res CobaltResponse
	if err = PostJSON(cobaltEndpoint, &req, &res); err != nil {
		return "", err
	}

	switch res.Status {
	case "redirect":
		return download(res.Url, filename)
	case "stream":
		return download(res.Url, filename)
	case "picker":
		return downloadPicker(&res, filename)
	case "error":
		return "", errors.New(res.Text)
	default:
		return "", fmt.Errorf("status not supported %v", res.Status)
	}
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
			//delete(cache, fn)
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

func download(url string, filename string) (path string, err error) {
	// Get the data
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.2 Safari/605.1.15")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("code %v", resp.StatusCode)
	}

	path, err = getFilename(filename, resp.Header.Get("Content-Type"))
	if err != nil {
		return "", err
	}

	out, err := os.OpenFile(path, os.O_TRUNC|os.O_WRONLY|os.O_CREATE, os.ModePerm)
	if err != nil {
		return "", err
	}
	defer out.Close()

	// Writer the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return "", err
	}

	return "", nil
}

var mimeExtension = map[string]string{
	"video/mp4":  ".mp4",
	"image/gif":  ".gif",
	"image/jpeg": ".jpg",
	"image/png":  ".png",
}

func PostJSON(url string, req any, res any) error {
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return err
	}

	r, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(reqBytes))

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return err
	}

	return nil
}

func downloadPicker(res *CobaltResponse, filename string) (string, error) {
	path, _ := getFilename(filename, "video/mp4")

	var cmd *exec.Cmd
	if res.Audio != "" {
		cmd = exec.Command("ffmpeg", "-f", "concat", "-safe", "0", "-protocol_whitelist", "file,https,tcp,tls,pipe,fd", "-i", "-", "-i", res.Audio, "-shortest", "-vsync", "vfr", "-pix_fmt", "yuv420p", "-y", "-loglevel", "warning", path)
	} else {
		cmd = exec.Command("ffmpeg", "-f", "concat", "-safe", "0", "-protocol_whitelist", "file,https,tcp,tls,pipe,fd", "-i", "-", "-vsync", "vfr", "-pix_fmt", "yuv420p", "-y", "-loglevel", "warning", path)
	}

	out := new(bytes.Buffer)
	input := formatThing(res)
	cmd.Stdin = input
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg err: %v: %v", err, out.String())
	}

	return path, nil
}

func formatThing(res *CobaltResponse) *bytes.Buffer {
	var out bytes.Buffer

	for _, p := range res.Picker {
		out.WriteString("file '")
		out.WriteString(p.Url)
		out.WriteString("'\n")
		out.WriteString("duration 2.5\n")
	}
	out.WriteString("file '")
	out.WriteString(res.Picker[len(res.Picker)-1].Url)
	out.WriteString("'\n")

	return &out
}
