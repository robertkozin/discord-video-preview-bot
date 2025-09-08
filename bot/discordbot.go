package bot

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/robertkozin/discord-video-preview-bot/preview"
	"go.opentelemetry.io/otel"

	"github.com/bwmarrin/discordgo"
)

var (
	tracer     = otel.Tracer("bot")
	urlPattern = regexp.MustCompile(`https://\S+`)
)

type Discord struct {
	id                 string
	session            *discordgo.Session
	inFlightMessages   map[string]chan *discordgo.Message
	lastChannelMessage map[string]string

	Token      string
	Reuploader preview.Reuploader
}

func (b *Discord) Start() error {
	dg, _ := discordgo.New("Bot " + b.Token)
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

func (b *Discord) Close() error {
	return b.session.Close()
}

func (b *Discord) readyHandler(s *discordgo.Session, m *discordgo.Ready) {
	b.id = m.User.ID
	if b.lastChannelMessage == nil {
		b.lastChannelMessage = make(map[string]string)
	}
}

func (b *Discord) messageCreateHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	b.lastChannelMessage[m.ChannelID] = m.ID

	if m.Author.ID == b.id {
		return
	}

	url := urlPattern.FindString(m.Content)
	if url == "" {
		return
	}

	if !b.Reuploader.IsSupported(url) {
		return
	}

	ctx := context.Background()
	go b.replyToMessage(ctx, s, m, url)
}

func (b *Discord) replyToMessage(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, url string) {
	ctx, span := tracer.Start(ctx, "discord_reply")
	defer span.End()

	embedCh := b.waitForEmbed(m)

	hostedURLs, err := b.Reuploader.Reupload(ctx, url)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(1, err.Error())
		return
	}

	reply := b.buildReply(<-embedCh, hostedURLs)

	go b.HideEmbeds(m.ChannelID, m.ID)

	// if there has been a message since then, reply
	if lastMsg := b.lastChannelMessage[m.ChannelID]; lastMsg != m.ID {
		reply.Reference = m.Reference()
	}

	_, err = s.ChannelMessageSendComplex(m.ChannelID, reply)
	if err != nil {
		slog.Error("channel send message", "channel_id", m.ChannelID, "reply", reply, "url", url, "err", err)
	}
}

func (b *Discord) waitForEmbed(mc *discordgo.MessageCreate) <-chan *discordgo.MessageEmbed {
	ret := make(chan *discordgo.MessageEmbed, 1)

	if len(mc.Embeds) > 0 {
		ret <- mc.Embeds[0]
		close(ret)
		return ret
	}

	go func() {
		defer close(ret)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
		defer cancel()
		removeHandler := b.session.AddHandler(func(s *discordgo.Session, mu *discordgo.MessageUpdate) {
			if mu.ID != mc.ID || len(mu.Embeds) == 0 {
				return
			}
			select {
			case <-ctx.Done():
				return
			case ret <- mu.Embeds[0]:
				cancel()
			}
		})
		defer removeHandler()
		<-ctx.Done()
	}()

	return ret
}

func (b *Discord) buildReply(embed *discordgo.MessageEmbed, hostedURLs []string) *discordgo.MessageSend {
	content := formatEmbedAsText(embed)

	galleryItems := make([]discordgo.MediaGalleryItem, len(hostedURLs))
	for i, hostedURL := range hostedURLs {
		galleryItems[i].Media = discordgo.UnfurledMediaItem{
			URL: hostedURL,
		}
	}

	messageSend := &discordgo.MessageSend{
		Components: []discordgo.MessageComponent{
			discordgo.TextDisplay{
				Content: content,
			},
			discordgo.MediaGallery{
				Items: galleryItems,
			},
		},
		Flags: discordgo.MessageFlagsIsComponentsV2,
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse:       []discordgo.AllowedMentionType{},
			RepliedUser: true,
		},
	}

	return messageSend
}

func (b *Discord) messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if updateCh, ok := b.inFlightMessages[m.ID]; ok {
		updateCh <- m.Message
	}
}

func (b *Discord) HideEmbeds(channelID, msgID string) {
	_, _ = b.session.RequestWithBucketID("PATCH", discordgo.EndpointChannelMessage(channelID, msgID), map[string]int{"flags": 4}, discordgo.EndpointChannelMessage(channelID, ""))
}

func formatEmbedAsText(embed *discordgo.MessageEmbed) string {
	if embed == nil {
		return ""
	}

	var (
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

	desc = embed.Description
	desc = preventURLEmbeds(desc)
	lines := strings.Split(desc, "\n")
	if len(lines) >= 1 {
		desc = lines[0]
	}

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
