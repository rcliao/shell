package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Slash commands (B2). The bridge's commands already work typed as text;
// registering them gives the family a menu with descriptions instead of
// commands they must remember. The same bridge handler runs either way.

// slashCommand is one registered command: its bridge name, the description
// the menu shows, and an optional text argument.
type slashCommand struct {
	name, description string
	arg, argHelp      string // "" = no argument
	argRequired       bool
}

// slashCommands is the family-facing set. Operator commands (plan, pm,
// tunnel, heartbeat, ...) stay text-only: a menu entry is an invitation.
var slashCommands = []slashCommand{
	{name: "new", description: "Start a fresh conversation here"},
	{name: "status", description: "Show this conversation's session"},
	{name: "help", description: "What I can do"},
	{name: "remember", description: "Save something to my memory", arg: "text", argHelp: "What to remember", argRequired: true},
	{name: "forget", description: "Remove something from my memory", arg: "text", argHelp: "What to forget", argRequired: true},
	{name: "memories", description: "List what I remember"},
	{name: "projects", description: "List our projects"},
	{name: "schedule", description: "List or manage reminders and schedules", arg: "args", argHelp: "Leave empty to list"},
	{name: "reactions", description: "Which reactions do what"},
}

// applicationCommands renders the set for registration: usable in the
// server and in DMs with the bot.
func applicationCommands() []*discordgo.ApplicationCommand {
	contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild, discordgo.InteractionContextBotDM}
	var out []*discordgo.ApplicationCommand
	for _, c := range slashCommands {
		ac := &discordgo.ApplicationCommand{Name: c.name, Description: c.description, Contexts: &contexts}
		if c.arg != "" {
			ac.Options = []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: c.arg,
				Description: c.argHelp, Required: c.argRequired,
			}}
		}
		out = append(out, ac)
	}
	return out
}

// registerCommands replaces this bot's global commands with the set above.
// A bulk overwrite is idempotent, so running it on every connect is safe.
func (b *Bot) registerCommands(appID string) {
	if b.session == nil || appID == "" {
		return
	}
	if _, err := b.session.ApplicationCommandBulkOverwrite(appID, "", applicationCommands()); err != nil {
		slog.Warn("discord: slash command registration failed — typed commands still work", "error", err)
		return
	}
	slog.Info("discord: slash commands registered", "count", len(slashCommands))
}

// handleSlash runs a slash command through the bridge, like a typed one.
func (h *Handler) handleSlash(ctx context.Context, i *discordgo.Interaction) {
	data := i.ApplicationCommandData()
	known := false
	for _, c := range slashCommands {
		if c.name == data.Name {
			known = true
		}
	}
	if !known {
		return
	}
	// Acknowledge first ("thinking…"): some commands take longer than 3 s.
	if err := h.api.Respond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource}); err != nil {
		slog.Warn("discord: acknowledge slash command", "error", err)
		return
	}
	user := interactionUser(i)
	if user == nil {
		return
	}
	userID, linked := h.addr.User(user.ID)
	if !linked {
		h.answerSlash(i, "I don't know you yet. Ask the owner to link your Discord id "+user.ID+".")
		return
	}
	w, err := h.locate(i.ChannelID)
	if err != nil {
		h.answerSlash(i, "I can't tell which conversation this is.")
		return
	}
	if h.authorize != nil && !h.authorize(userID, w.conv.ChatID, !w.isDM) {
		h.answerSlash(i, "Sorry, you can't use that here.")
		return
	}
	var args []string
	for _, o := range data.Options {
		if o.Type == discordgo.ApplicationCommandOptionString {
			args = append(args, o.StringValue())
		}
	}
	resp, err := h.bridge.HandleCommand(ctx, w.conv.ChatID, w.conv.ThreadID, data.Name, strings.Join(args, " "))
	if err != nil {
		slog.Error("discord: slash command failed", "cmd", data.Name, "error", err)
		resp = "Command failed: " + err.Error()
	}
	h.answerSlash(i, resp)
}

// answerSlash fills the deferred reply: the first chunk replaces "thinking…",
// the rest follow.
func (h *Handler) answerSlash(i *discordgo.Interaction, text string) {
	if strings.TrimSpace(text) == "" {
		text = "Done."
	}
	chunks := splitMessage(fenceTables(text), maxMessageLen)
	first, _ := h.bot.mentions.Render(chunks[0])
	if err := h.api.EditResponse(i, &discordgo.WebhookEdit{Content: &first, AllowedMentions: noPings()}); err != nil {
		slog.Warn("discord: slash reply", "error", err)
	}
	for _, c := range chunks[1:] {
		c, _ = h.bot.mentions.Render(c)
		if err := h.api.Followup(i, &discordgo.WebhookParams{Content: c, AllowedMentions: noPings()}); err != nil {
			slog.Warn("discord: slash follow-up", "error", err)
		}
	}
}
