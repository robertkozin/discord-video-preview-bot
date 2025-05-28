package main

import (
	"fmt"
	"github.com/bwmarrin/discordgo"
	"log/slog"
	urlpkg "net/url"
	"path/filepath"
	"preview-bot/downloader"
	"regexp"
	"time"
)

var (
	urlPattern        = regexp.MustCompile(`https://\S+`)
	urlParamAllowList = map[string][]string{
		"youtube.com": {"v", "t"},
	}
)

type Bot struct {
	id      string
	session *discordgo.Session

	inFlightMessages map[string]chan *discordgo.Message

	PublicURL   string
	Downloaders *downloader.DownloaderRegistry
}

func (b *Bot) Start(token string) error {
	dg, _ := discordgo.New("Bot " + token)
	b.session = dg

	dg.Identify.Intents = discordgo.IntentGuildMessages | discordgo.IntentDirectMessages

	dg.SyncEvents = true
	dg.StateEnabled = false

	dg.AddHandler(b.readyHandler)
	dg.AddHandler(b.messageCreateHandler)
	dg.AddHandler(b.messageUpdate)

	var err error
	err = dg.Open()
	if err != nil {
		return fmt.Errorf("dg.Open: %w", err)
	}

	return nil
}

func (b *Bot) Stop() {
	_ = b.session.Close()
}

func (b *Bot) readyHandler(s *discordgo.Session, m *discordgo.Ready) {
	b.id = m.User.ID
	if b.inFlightMessages == nil {
		b.inFlightMessages = make(map[string]chan *discordgo.Message)
	}
}

func (b *Bot) messageCreateHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == b.id {
		return
	}

	url := urlPattern.FindString(m.Content)
	if url == "" {
		return
	}

	if !b.Downloaders.HasMatch(url) {
		return
	}

	go b.replyToMessage(s, m, url)
}

func (b *Bot) replyToMessage(s *discordgo.Session, m *discordgo.MessageCreate, url string) {
	cleanUrl, err := cleanURLParams(url, urlParamAllowList)

	contentChan := b.getContent(m, url)

	filename, err := b.Downloaders.Download(cleanUrl)
	if err != nil || filename == "" {
		return
	}

	content, _ := <-contentChan

	reply := b.formatContentAndDownload(content, filename)

	b.HideEmbeds(m.ChannelID, m.ID)

	_, err = s.ChannelMessageSend(m.ChannelID, reply)
	if err != nil {
		slog.Error("channel send message", "channel_id", m.ChannelID, "reply", reply, "url", url, "err", err)
	}
}

func (b *Bot) getContent(m *discordgo.MessageCreate, url string) chan string {
	ch := make(chan string, 1)

	go func() {
		defer close(ch)
		if len(m.Embeds) > 0 {
			ch <- formatEmbedAsText(m.Embeds)
		}

		updatedMessageCh := make(chan *discordgo.Message)
		b.inFlightMessages[m.ID] = updatedMessageCh
		defer close(updatedMessageCh)
		defer delete(b.inFlightMessages, m.ID)

		select {
		case msg := <-updatedMessageCh:
			ch <- formatEmbedAsText(msg.Embeds)
		case <-time.After(time.Second * 3):
			ch <- ""
		}
	}()

	return ch
}

func (b *Bot) formatContentAndDownload(content string, filename string) string {
	base := filepath.Base(filename)
	downloadURL, err := urlpkg.JoinPath(b.PublicURL, base)
	if err != nil {
		slog.Error("error formatting public download url", "content", content, "filename", filename, "base", base, "public_url", b.PublicURL, "err", err)
		panic("error formatting public download url")
	}

	return content + "[.](" + downloadURL + ")"
}

func (b *Bot) messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if updateCh, ok := b.inFlightMessages[m.ID]; ok {
		updateCh <- m.Message
	}
}

func (b *Bot) HideEmbeds(channelID, msgID string) {
	_, _ = b.session.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(channelID, msgID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(channelID, ""))
}

func formatEmbedAsText(embeds []*discordgo.MessageEmbed) string {
	if len(embeds) == 0 {
		return ""
	}

	var (
		embed = embeds[0]
		title string
		desc  string
	)

	if embed.Title != "" {
		title = embed.Title
	} else {
		if embed.Author != nil && embed.Author.Name != "" {
			title = embed.Author.Name
		} else if embed.Provider != nil && embed.Provider.Name != "" {
			title = embed.Provider.Name
		}
	}

	desc = preventURLEmbeds(embed.Description)

	if title != "" && desc != "" {
		return "**" + title + "**" + " " + desc
	} else if title != "" {
		return "**" + title + "**"
	} else if desc != "" {
		return desc
	}

	return ""
}

var _urlPattern = regexp.MustCompile(`https?://\S+`)

func preventURLEmbeds(url string) string {
	return _urlPattern.ReplaceAllString(url, `<$0>`)
}

func cleanURLParams(rawURL string, allowList map[string][]string) (string, error) {
	url, err := urlpkg.Parse(rawURL)
	if err != nil {
		return "", err
	}
	allow, _ := allowList[url.Hostname()]
	oldv := url.Query()
	newv := urlpkg.Values{}
	for _, a := range allow {
		newv[a] = oldv[a]
	}
	url.RawQuery = newv.Encode()
	url.Fragment = ""
	return url.String(), nil
}
