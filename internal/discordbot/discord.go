package discordbot

import (
	"context"
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

// Discord adapts discordgo to Chat and wires gateway events to the Bot.
// The bot connects outbound over the gateway, so it needs no public endpoint.
type Discord struct {
	S         *discordgo.Session
	GuildID   string
	ChannelID string
}

func (d *Discord) CreateThread(_ context.Context, name, first string) (string, error) {
	msg, err := d.S.ChannelMessageSend(d.ChannelID, first)
	if err != nil {
		return "", err
	}
	th, err := d.S.MessageThreadStartComplex(d.ChannelID, msg.ID, &discordgo.ThreadStart{
		Name: name, AutoArchiveDuration: 10080, Type: discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		return "", err
	}
	return th.ID, nil
}

func (d *Discord) Send(_ context.Context, threadID string, m Message) error {
	content := m.Text
	if m.Code != "" {
		content += "\n```json\n" + m.Code + "\n```"
	}
	send := &discordgo.MessageSend{Content: content, AllowedMentions: &discordgo.MessageAllowedMentions{}}
	if len(m.Buttons) > 0 {
		var row []discordgo.MessageComponent
		for _, b := range m.Buttons {
			style := discordgo.SuccessButton
			if b.Danger {
				style = discordgo.DangerButton
			}
			row = append(row, discordgo.Button{Label: b.Label, Style: style, CustomID: b.ID})
		}
		send.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
	}
	_, err := d.S.ChannelMessageSendComplex(threadID, send)
	return err
}

var profileChoices = []*discordgo.ApplicationCommandOptionChoice{
	{Name: "plan", Value: "plan"}, {Name: "research", Value: "research"}, {Name: "action", Value: "action"},
	{Name: "mail", Value: "mail"}, {Name: "project", Value: "project"},
}

var classChoices = []*discordgo.ApplicationCommandOptionChoice{
	{Name: "P0 now", Value: "p0"}, {Name: "P1 normal", Value: "p1"}, {Name: "P2 background", Value: "p2"}, {Name: "P3 housekeeping", Value: "p3"},
}

// Commands are the guild slash commands.
var Commands = []*discordgo.ApplicationCommand{
	{Name: "task", Description: "Start a new aejar task", Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionString, Name: "title", Description: "Short title", Required: true},
		{Type: discordgo.ApplicationCommandOptionString, Name: "prompt", Description: "What the session should do", Required: true},
		{Type: discordgo.ApplicationCommandOptionString, Name: "class", Description: "Priority", Choices: classChoices},
		{Type: discordgo.ApplicationCommandOptionString, Name: "profile", Description: "Session profile", Choices: profileChoices},
	}},
	{Name: "status", Description: "Scheduler and limit status"},
	{Name: "tasks", Description: "Recent tasks"},
	{Name: "screen", Description: "What a task's session shows right now", Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionString, Name: "task", Description: "Task id", Required: true},
	}},
	{Name: "finish", Description: "Mark a task done and end its session", Options: []*discordgo.ApplicationCommandOption{
		{Type: discordgo.ApplicationCommandOptionString, Name: "task", Description: "Task id", Required: true},
	}},
}

// Wire registers commands and routes gateway events to b.
func (d *Discord) Wire(b *Bot) error {
	d.S.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	d.S.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		ctx := context.Background()
		user := ""
		if i.Member != nil && i.Member.User != nil {
			user = i.Member.User.ID
		}
		var reply string
		// Ack within Discord's 3 s window, then do the work.
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
		})
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			data := i.ApplicationCommandData()
			opts := map[string]string{}
			for _, o := range data.Options {
				opts[o.Name] = fmt.Sprint(o.Value)
			}
			reply = b.Command(ctx, user, data.Name, opts)
		case discordgo.InteractionMessageComponent:
			reply = b.Click(ctx, user, i.MessageComponentData().CustomID)
		}
		if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: reply, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: &discordgo.MessageAllowedMentions{},
		}); err != nil {
			log.Printf("discord followup: %v", err)
		}
	})
	d.S.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author == nil || m.Author.Bot {
			return
		}
		if reply, ok := b.ThreadMessage(context.Background(), m.Author.ID, m.ChannelID, m.Content); ok {
			s.ChannelMessageSendReply(m.ChannelID, reply, m.Reference())
		}
	})
	if err := d.S.Open(); err != nil {
		return err
	}
	_, err := d.S.ApplicationCommandBulkOverwrite(d.S.State.User.ID, d.GuildID, Commands)
	return err
}
