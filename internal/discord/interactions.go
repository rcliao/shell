package discord

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Interactions: button clicks (and, later, slash commands). Discord requires
// an acknowledgement within 3 seconds, so every handler answers "deferred"
// first and does the work after.

// replyActionPrefix marks a reply button's custom id: "act:<emoji>", where
// the emoji is the reaction the button stands for. A click runs exactly what
// that reaction would, so the two can never drift apart.
const replyActionPrefix = "act:"

// replyButtonSpecs are the buttons offered under a reply, when the agent's
// reaction map has the action.
var replyButtonSpecs = []struct{ emoji, label string }{
	{"🔄", "Regenerate"},
	{"📌", "Remember"},
}

// replyButtons builds the button row for a reply; nil when none apply.
func (h *Handler) replyButtons() []discordgo.MessageComponent {
	var row []discordgo.MessageComponent
	for _, b := range replyButtonSpecs {
		if h.bridge.ReactionAction(b.emoji) == "" {
			continue
		}
		row = append(row, discordgo.Button{
			Label:    b.label,
			Style:    discordgo.SecondaryButton,
			Emoji:    &discordgo.ComponentEmoji{Name: b.emoji},
			CustomID: replyActionPrefix + b.emoji,
		})
	}
	if len(row) == 0 {
		return nil
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// addComponents puts button rows under a delivered reply. Content is left as
// it is (a nil Content in an edit means "unchanged").
func (h *Handler) addComponents(channelID string, messageID int, rows []discordgo.MessageComponent) {
	if len(rows) == 0 {
		return
	}
	if _, err := h.api.Edit(&discordgo.MessageEdit{ID: itoa(messageID), Channel: channelID, Components: &rows}); err != nil {
		slog.Debug("discord: add reply components", "error", err)
	}
}

// HandleInteraction routes a slash command or a button click.
func (h *Handler) HandleInteraction(ctx context.Context, i *discordgo.Interaction) {
	if i != nil && i.Type == discordgo.InteractionApplicationCommand {
		h.handleSlash(ctx, i)
		return
	}
	if i == nil || i.Type != discordgo.InteractionMessageComponent || i.Message == nil {
		return
	}
	id := i.MessageComponentData().CustomID
	if strings.HasPrefix(id, pickPrefix) {
		h.handlePick(ctx, i, strings.TrimPrefix(id, pickPrefix))
		return
	}
	if !strings.HasPrefix(id, replyActionPrefix) {
		return
	}
	// Acknowledge first: the work (a regenerate is a whole turn) takes far
	// longer than the 3 s Discord allows before it shows "interaction failed".
	if err := h.api.Respond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		slog.Warn("discord: acknowledge button", "error", err)
	}
	user := interactionUser(i)
	if user == nil {
		return
	}
	// Same path as the reaction: auth, the exchange lookup, the result mark
	// and the reply.
	h.HandleReaction(ctx, &discordgo.MessageReaction{
		UserID:    user.ID,
		MessageID: i.Message.ID,
		ChannelID: i.ChannelID,
		Emoji:     discordgo.Emoji{Name: strings.TrimPrefix(id, replyActionPrefix)},
	})
}

// interactionUser is who clicked: a guild member, or the user in a DM.
func interactionUser(i *discordgo.Interaction) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

func itoa(n int) string { return strconv.Itoa(n) }
