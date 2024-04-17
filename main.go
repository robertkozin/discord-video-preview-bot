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
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

var discordToken = mustGetEnvString("DISCORD_TOKEN")
var previewDir = mustGetEnvString("PREVIEW_DIR")
var previewBaseUrl = mustGetEnvString("PREVIEW_BASE_URL")

var linkMatch = regexp.MustCompile(`https://\S+`)

var botID string

var history = make(map[string]string)

var inFlightMessages = make(map[string]*discordgo.Message)
var channelLastMessage = make(map[string]string)

var cobaltEndpoint = "https://co.wuk.sh/api/json"    // https://github.com/wukko/cobalt
var spvEndpoint = "https://spv.ncp.nathanferns.xyz/" // https://github.com/nathanielfernandes/spv

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
	Audio       any            `json:"audio"`
}

type CobaltPicker struct {
	Type  string `json:"type"` // video, used only if pickerTypeis various
	Url   string `json:"url"`
	Thumb string `json:"thumb"`
}

// Todo: Fix tiktok slideshow: https://www.tiktok.com/@ridingtojisface/video/7304371224690396447
// Todo: Make file already exists cache (after embed update after replied)
// Todo: download things from picker instead of using url
// Todo: Support 'various' picker
// Todo: self host cobalt api
// Todo: message getting an embed after posted and update reply with embed data
// Todo: don't reply if message got deleted

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
	dg.AddHandler(messageUpdate)

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

func getFilename(filename string, res *http.Response) (string, error) {
	if cd := res.Header.Get("Content-Disposition"); cd != "" {
		_, params, _ := mime.ParseMediaType(cd)
		if name, ok := params["filename"]; ok {
			ext := filepath.Ext(name)
			ext = strings.ToLower(ext)
			for _, v := range mimeExtension {
				if ext == v {
					return filename + ext, nil
				}
			}
		}
	}

	mediatype, _, err := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if err != nil {
		return "", fmt.Errorf("error parsing mimetype: %v: %v", res.Header.Get("Content-Type"), err)
	}
	ext, ok := mimeExtension[mediatype]
	if !ok {
		return "", fmt.Errorf("mimetype not supported: %v: %v", mediatype, err)
	}

	return filename + ext, nil
}

func messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if _, ok := inFlightMessages[m.ID]; ok {
		inFlightMessages[m.ID] = m.Message
	}
}

var matchUrl = regexp.MustCompile(`https://\S+`)

func replaceUrls(in string) string {
	return matchUrl.ReplaceAllString(in, `<$0>`)
}

var downloaders = []func(string) (string, error){ssvid, cobalt, spotify}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	channelLastMessage[m.ChannelID] = m.ID

	if m.Author.ID == botID {
		return
	}

	link := matchUrl.FindString(m.Content)
	if link == "" {
		return
	}

	inFlightMessages[m.ID] = m.Message
	defer delete(inFlightMessages, m.ID)

	//_ = s.ChannelTyping(m.ChannelID)

	var path string
	var err error
	for _, downloader := range downloaders {
		path, err = downloader(link)
		if err != nil {
			slog.Error("downloader", "link", link, "err", err)
		}
		if path != "" {
			break
		}
	}

	if path == "" {
		return
	}

	base := filepath.Base(path)
	reply := strings.Builder{}

	if newMsg, ok := inFlightMessages[m.ID]; ok && len(newMsg.Embeds) > 0 {
		embed := newMsg.Embeds[0]
		if embed.Title == "" {
			if embed.Author != nil && embed.Author.Name != "" {
				embed.Title = embed.Author.Name
			} else if embed.Provider != nil && embed.Provider.Name != "" {
				embed.Title = embed.Provider.Name
			} else {
				embed.Title = ""
			}
		}

		reply.WriteString("**")
		reply.WriteString(embed.Title)
		reply.WriteString("**")

		if embed.Description != "" {
			reply.WriteByte(' ')
			reply.WriteString(replaceUrls(embed.Description))
		}

	}

	reply.WriteString("[.](")
	reply.WriteString(previewBaseUrl)
	reply.WriteString(base)
	reply.WriteByte(')')

	_, _ = s.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(m.ChannelID, m.ID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(m.ChannelID, ""))

	var newMsg *discordgo.Message
	if id, ok := channelLastMessage[m.ChannelID]; ok && id == m.ID {
		newMsg, err = s.ChannelMessageSend(m.ChannelID, reply.String())
	} else {
		newMsg, err = s.ChannelMessageSendReply(m.ChannelID, reply.String(), m.Reference())
	}
	if err != nil {
		slog.Error("err sending message", "err", err)
		return
	}

	// add message to history
	history[m.ID] = newMsg.ID
}

func messageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	if id, ok := history[m.ID]; ok {
		_ = s.ChannelMessageDelete(m.ChannelID, id)
		delete(history, m.ID)
	}
}

var matchSpotifyTrack = regexp.MustCompile(`spotify\.com/track/([a-zA-Z0-9]+)\S+`)

// TODO: Improve this to include short links
var previewMatch = regexp.MustCompile(`\S+(?:tiktok\.com|instagram\.com|twitter\.com|://t\.co|reddit\.com|redd\.it|clips\.twitch\.tv|youtube.com/shorts/|://x.com|spotify.com/track/)\S+`)

var isCobalt = filterUrls(
	`tiktok\.com`,
	`instagram\.com`,
	`twitter\.com`,
	`://t\.co`,
	`reddit.com`,
	`redd.it`,
	`clips\.twitch\.tv`,
	`youtube\.com/shorts/`,
	`://x\.com`,
)

func cobalt(url string) (path string, err error) {
	if !isCobalt(url) {
		return "", nil
	}

	filename := sha7(url) // First 7 chars of sha1 hash of url

	var req = CobaltRequest{
		Url: url,
	}
	var res CobaltResponse
	if err = PostJSON(cobaltEndpoint, &req, &res); err != nil {
		return "", err
	}

	switch res.Status {
	case "redirect":
		filename = filepath.Join(previewDir, filename)
		return download(res.Url, filename)
	case "stream":
		filename = filepath.Join(previewDir, filename)
		return download(res.Url, filename)
	case "picker":
		return downloadPicker(&res, filename)
	case "error":
		return "", errors.New(res.Text)
	default:
		return "", fmt.Errorf("status not supported %v", res.Status)
	}
}

func spotify(u string) (string, error) {
	if m := matchSpotifyTrack.FindStringSubmatch(u); len(m) != 0 {
		filename := sha7(u)
		return downloadSpotify(filename, m[1])
	}
	return "", nil
}

func downloadSpotify(filename string, id string) (string, error) {
	path := filepath.Join(previewDir, filename+".mp4")

	res := &struct {
		CanvasUrl string `json:"canvas_url,omitempty"`
		AudioUrl  string `json:"audio_url"`
	}{}
	GetJSON(spvEndpoint+id+"/info", res)

	hasCanvas := res.CanvasUrl != ""
	if hasCanvas && strings.HasSuffix(res.CanvasUrl, ".mp4") {
		imagePath, err := download(spvEndpoint+id+"?overlay=1", id+"_temp")

		if err != nil {
			return "", err
		}

		defer os.Remove(imagePath)

		cmd := exec.Command(
			"ffmpeg",
			"-y",
			"-stream_loop", "-1",
			"-i", res.CanvasUrl,
			"-i", res.AudioUrl,
			"-i", imagePath,
			"-filter_complex", "[0:v]scale=720:1280[v];[v][2:v] overlay=25:25:enable='between(t,0,15)'",
			"-map", "0:v:0",
			"-map", "1:a:0",
			"-t", "30",
			"-shortest",
			"-c:v", "libx264",
			"-preset", "superfast",
			"-movflags", "faststart",
			"-r", "24",
			"-pix_fmt", "yuv420p",
			path,
		)

		out := bytes.Buffer{}
		cmd.Stdout = &out
		cmd.Stderr = &out

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg err: %v: %v", err, out.String())
		}

		return path, nil
	} else {
		// download image
		imageUrl := spvEndpoint + id
		if hasCanvas {
			imageUrl += "?overlay=1"
		}

		imagePath, err := download(imageUrl, id+"_image")
		if err != nil {
			return "", err
		}
		defer os.Remove(imagePath)

		cmd := exec.Command(
			"ffmpeg",
			"-y",
			"-loop", "1",
			"-i", imagePath,
			"-i", res.AudioUrl,
			"-tune", "stillimage",
			"-t", "30",
			"-shortest",
			"-c:a", "aac",
			"-preset", "superfast",
			"-movflags", "faststart",
			"-r", "24",
			"-pix_fmt", "yuv420p",
			path,
		)

		out := bytes.Buffer{}
		cmd.Stdout = &out
		cmd.Stderr = &out

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg err: %v: %v", err, out.String())
		}

		return path, nil
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
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("code %v", resp.StatusCode)
	}

	path, err = getFilename(filename, resp)
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

	return path, nil
}

func downloadConcurrent(urls []string, filenames []string) (paths []string, all_ok bool) {
	all_ok = true
	paths = make([]string, len(urls))
	for i, url := range urls {
		path, err := download(url, filenames[i])
		if err != nil {
			all_ok = false
		}
		paths[i] = path
	}

	return paths, all_ok
}

var mimeExtension = map[string]string{
	"video/mp4":  ".mp4",
	"image/gif":  ".gif",
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"audio/mpeg": ".mp3",
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

func GetJSON(url string, res any) error {
	r, _ := http.NewRequest(http.MethodGet, url, nil)

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
	fmt.Printf("%+v\n", res)
	if s, ok := res.Audio.(string); ok && s != "" {
		// final path
		path := filepath.Join(previewDir, filename+".mp4")

		// make temp dir for all artifacts
		tempDir, err := os.MkdirTemp(previewDir, "picker*")
		if err != nil {
			return "", fmt.Errorf("err creating temp dir: %v", err)
		}
		defer os.RemoveAll(tempDir)

		filename = filepath.Join(tempDir, filename)

		audioPath, err := download(s, filename+"_audio")
		if err != nil {
			return "", fmt.Errorf("err downloading audio: %v", err)
		}

		videoPath, err := buildPickerVideo(res, tempDir)
		if err != nil {
			return "", fmt.Errorf("err building video: %v", err)
		}

		cmd := exec.Command("ffmpeg",
			"-i", videoPath,
			"-i", audioPath,
			"-c:v", "copy",
			"-c:a", "aac",
			"-shortest",
			"-y", path,
		)

		out := new(bytes.Buffer)
		cmd.Stdout = out
		cmd.Stderr = out

		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("ffmpeg err: %v: %v", err, out.String())
		}

		if out.Len() > 0 {
			slog.Error("ffmpeg output", "out", out.String())
		}

		return path, nil
	} else if _, ok = res.Audio.(bool); ok {
		return buildPickerVideo(res, filename)
	} else {
		return "", fmt.Errorf("no match for picker: %+v", res)
	}
}

func buildPickerVideo(res *CobaltResponse, dir string) (string, error) {
	// only video do not have audio
	filename := sha7(res.Url)
	path := filepath.Join(dir, filename+"_images.mp4")
	length := strconv.FormatFloat(float64(len(res.Picker))*2.5, 'f', -1, 64)

	seq := formatSequentialThing(res, dir)
	cmd := exec.Command("ffmpeg",
		"-framerate", "0.4", // 1 frame every 2.5 seconds
		"-start_number", "0",
		"-i", seq,
		"-c:v", "libx264",
		"-tune", "stillimage",
		"-preset", "ultrafast",
		"-c:a", "aac",
		// allow for any width and height
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p",
		"-shortest",
		"-t", length, // Set the total video duration as needed
		"-y", path,
	)

	out := new(bytes.Buffer)
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffmpeg err: %v: %v", err, out.String())
	}

	if out.Len() > 0 {
		slog.Error("ffmpeg output", "out", out.String())
	}

	return path, nil
}

func formatConcatThing(res *CobaltResponse) *bytes.Buffer {
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

func formatSequentialThing(res *CobaltResponse, dir string) string {
	base := filepath.Join(dir, sha7(res.Url))

	filenames := make([]string, len(res.Picker))
	urls := make([]string, len(res.Picker))

	for i, p := range res.Picker {
		filenames[i] = base + "_" + strconv.Itoa(i)
		urls[i] = p.Url
	}

	paths, ok := downloadConcurrent(urls, filenames)

	if len(paths) == 0 || !ok {
		return ""
	}

	ext := filepath.Ext(paths[0])

	return base + "_%d" + ext
}

func sha7(s string) string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(s)))[:7] // First 7 chars of sha1 hash of s
}

type ssvidResponse struct {
	Status string `json:"status"`
	Mess   string `json:"mess"`
	P      string `json:"p"`
	Data   struct {
		Page      string `json:"page"`
		Extractor string `json:"extractor"`
		Status    string `json:"status"`
		Keyword   string `json:"keyword"`
		Title     string `json:"title"`
		Thumbnail string `json:"thumbnail"`
		Pid       string `json:"pid"`
		Links     struct {
			Video []struct {
				QText string `json:"q_text"`
				Size  string `json:"size"`
				URL   string `json:"url"`
			} `json:"video"`
		} `json:"links"`
		Author struct {
			Username string `json:"username"`
			FullName string `json:"full_name"`
			Avatar   string `json:"avatar"`
		} `json:"author"`
	} `json:"data"`
}

var isSsvid = filterUrls(
	`instagram\.com`,
	`tiktok\.com`,
)

func ssvid(u string) (path string, err error) {
	if !isSsvid(u) {
		return "", nil
	}

	data := url.Values{
		"query": {u},
		"vt":    {"home"},
	}
	req, _ := http.NewRequest("POST", "https://www.ssvid.net/api/ajax/search", strings.NewReader(data.Encode()))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.3.1 Safari/605.1.15")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Host", "www.ssvid.net")
	req.Header.Set("Origin", "https://www.ssvid.net")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json")

	body, _ := httputil.DumpRequest(req, true)
	fmt.Println(string(body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		printResponse(resp)
		return "", err
	}
	defer resp.Body.Close()
	printResponse(resp)

	var ssvidResp ssvidResponse
	err = json.NewDecoder(resp.Body).Decode(&ssvidResp)
	if err != nil {
		return "", err
	}

	if len(ssvidResp.Data.Links.Video) == 0 {
		return "", fmt.Errorf("no videos in response")
	}

	filename := filepath.Join(previewBaseUrl, sha7(u))
	return download(ssvidResp.Data.Links.Video[0].URL, filename)
}

func printResponse(resp *http.Response) {
	b, _ := httputil.DumpResponse(resp, true)
	fmt.Println(string(b))
}

func filterUrls(u ...string) func(string) bool {
	filters := make([]*regexp.Regexp, len(u))
	for i := 0; i < len(u); i++ {
		filters[i] = regexp.MustCompile(u[i])
	}
	return func(s string) bool {
		for _, filter := range filters {
			if filter.MatchString(s) {
				return true
			}
		}
		return false
	}
}
