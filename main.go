package main

import (
	"crypto/md5"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"syscall"

	"github.com/bwmarrin/discordgo"
)

var DiscordToken = mustGetEnvString("DISCORD_TOKEN")
var PreviewDir = mustGetEnvString("PREVIEW_DIR")
var PreviewBaseUrl = mustGetEnvString("PREVIEW_BASE_URL")

var PreviewMatch = regexp.MustCompile(`\S+(?:tiktok\.com|instagram\.com|twitter\.com|reddit\.com)\S+`)

var BotId string

func main() {
	os.MkdirAll(PreviewDir, os.ModePerm)

	dg, _ := discordgo.New("Bot " + DiscordToken)

	dg.Identify.Intents = discordgo.IntentsGuildMessages
	dg.Identify.LargeThreshold = 50
	dg.Identify.GuildSubscriptions = false

	dg.SyncEvents = false
	dg.StateEnabled = false

	dg.AddHandler(ready)
	dg.AddHandler(messageCreate)

	err := dg.Open()
	if err != nil {
		log.Fatalln(err)
	}

	log.Println("Bot is now running. Press CTRL-C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc

	dg.Close()
}

func ready(s *discordgo.Session, m *discordgo.Ready) {
	BotId = m.User.ID
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == BotId {
		return
	}

	link := PreviewMatch.FindString(m.Content)
	if link == "" {
		return
	}

	output := preview(link)
	if output == "" {
		return
	}

	s.ChannelMessageSendReply(m.ChannelID, PreviewBaseUrl+output, m.MessageReference)
}

func preview(url string) (path string) {
	hashUrl := fmt.Sprintf("%x", md5.Sum([]byte(url)))

	outputFile := hashUrl + ".mp4"

	cmd := exec.Command(
		"yt-dlp",
		"--downloader", "ffmpeg", // Ffmpeg lets us limit video duration vs native downloader
		"--downloader-args", "ffmpeg:-to 60 -loglevel warning", // Limit to 60s
		"-S", "+vcodev:avc", // Prefer H264
		// Assume that the places we're downloading from already optimize for the web (faststart + H264)
		"--no-playlist",
		"-o", outputFile,
		"-P", PreviewDir,
		url,
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return ""
	}

	return outputFile
}

func mustGetEnvString(key string) (value string) {
	value, ok := os.LookupEnv(key)
	if !ok {
		panic("missing env var " + key)
	}

	return value
}
