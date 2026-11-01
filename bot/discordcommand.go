package bot

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// previewCommandName is shown under right click > Apps on a message.
const previewCommandName = "Video Preview"

var previewCommand = &discordgo.ApplicationCommand{
	Type: discordgo.MessageApplicationCommand,
	Name: previewCommandName,
}

// registerCommands creates (or updates, matched by name) the global message
// context menu command. Global commands can take a while to show up in clients.
func (b *Discord) registerCommands(appID string) {
	_, err := b.session.ApplicationCommandCreate(appID, "", previewCommand)
	if err != nil {
		fmt.Printf("registering %q command: %v\n", previewCommandName, err)
	}
}

func (b *Discord) interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	if data.Name != previewCommandName || data.Resolved == nil {
		return
	}
	msg := data.Resolved.Messages[data.TargetID]
	if msg == nil {
		return
	}
	// resolved messages don't carry the channel/guild ids
	msg.ChannelID = i.ChannelID
	msg.GuildID = i.GuildID

	url := urlPattern.FindString(msg.Content)
	if url == "" || !b.Reuploader.IsSupported(url) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "No supported link found in that message.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// reuploading takes longer than the 3s Discord allows for a response
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		fmt.Printf("deferring interaction: %v\n", err)
		return
	}

	go b.respondWithPreview(context.Background(), s, i.Interaction, msg, url)
}

func (b *Discord) respondWithPreview(ctx context.Context, s *discordgo.Session, i *discordgo.Interaction, msg *discordgo.Message, url string) {
	var err error
	ctx, span := tracer.Start(ctx, "discord_command", trace.WithAttributes(
		attribute.String("guild_id", i.GuildID),
		attribute.String("channel_id", i.ChannelID),
		attribute.String("message_id", msg.ID),
	))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			b.replaceWithError(s, i, err)
		}
		span.End()
	}()

	hostedURLs, err := b.Reuploader.Reupload(ctx, url)
	if err != nil {
		return
	}

	var embed *discordgo.MessageEmbed
	if len(msg.Embeds) > 0 {
		embed = msg.Embeds[0]
	}
	reply := b.buildReply(embed, hostedURLs)

	// discordgo's WebhookEdit has no flags field, which components v2 needs
	_, err = s.RequestWithBucketID("PATCH",
		discordgo.EndpointInteractionResponseActions(i.AppID, i.Token),
		map[string]any{
			"flags":            reply.Flags,
			"components":       reply.Components,
			"allowed_mentions": reply.AllowedMentions,
		},
		discordgo.EndpointInteractionResponseActions(i.AppID, ""),
	)
	if err != nil {
		err = fmt.Errorf("editing interaction response: %w", err)
		return
	}

	go b.HideEmbeds(msg.ChannelID, msg.ID)
}

// replaceWithError swaps the public "thinking..." message for an ephemeral error.
func (b *Discord) replaceWithError(s *discordgo.Session, i *discordgo.Interaction, err error) {
	content := "Couldn't preview that link: " + err.Error()
	if len(content) > 2000 {
		content = content[:1997] + "..."
	}
	_ = s.InteractionResponseDelete(i)
	_, _ = s.FollowupMessageCreate(i, false, &discordgo.WebhookParams{
		Content: content,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
}
