package discord

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// Answer buttons (B3, an experiment behind discord.answer_buttons). When the
// agent asks a question with a few clear options it may end the reply with
//
//	```choices
//	["Tacos", "Ramen", "Stay in"]
//	```
//
// and Discord shows them as buttons. A tap posts "<name> picked: Tacos" and
// runs as that person's reply to the question — reply quoting (B6) hands the
// agent the question with the answer. The buttons then disable, so an answer
// can't be sent twice.

var choicesBlockRe = regexp.MustCompile("(?s)```choices[ \t]*\n(.*?)\n?```[ \t]*\n?")

const (
	maxChoices     = 5
	maxChoiceRunes = 80
	pickPrefix     = "pick:"
)

// extractChoices lifts a choices block out of the text. A block that does
// not parse to one to five short strings stays as written.
func extractChoices(text string) (string, []string) {
	if !strings.Contains(text, "```choices") {
		return text, nil
	}
	var choices []string
	rest := choicesBlockRe.ReplaceAllStringFunc(text, func(block string) string {
		if choices != nil {
			return block // one set of buttons per reply
		}
		var raw []string
		if err := json.Unmarshal([]byte(choicesBlockRe.FindStringSubmatch(block)[1]), &raw); err != nil {
			return block
		}
		seen := map[string]bool{}
		var out []string
		for _, c := range raw {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] || utf8.RuneCountInString(c) > maxChoiceRunes {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
		if len(out) == 0 || len(out) > maxChoices {
			return block
		}
		choices = out
		return ""
	})
	return strings.TrimSpace(rest), choices
}

// choiceRow renders the options as buttons.
func choiceRow(choices []string, disabled bool, picked string) []discordgo.MessageComponent {
	if len(choices) == 0 {
		return nil
	}
	var row []discordgo.MessageComponent
	for _, c := range choices {
		style := discordgo.PrimaryButton
		if disabled {
			style = discordgo.SecondaryButton
			if c == picked {
				style = discordgo.SuccessButton
			}
		}
		row = append(row, discordgo.Button{Label: c, Style: style, CustomID: pickPrefix + c, Disabled: disabled})
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// finalComponents is every button row a reply gets: its choices, then the
// reply buttons.
func (h *Handler) finalComponents(choices []string) []discordgo.MessageComponent {
	rows := choiceRow(choices, false, "")
	if h.buttons {
		rows = append(rows, h.replyButtons()...)
	}
	return rows
}

// pickedChoices reads the choice labels back from a message's buttons.
func pickedChoices(m *discordgo.Message) []string {
	var out []string
	var walk func(cs []discordgo.MessageComponent)
	walk = func(cs []discordgo.MessageComponent) {
		for _, c := range cs {
			switch v := c.(type) {
			case *discordgo.ActionsRow:
				walk(v.Components)
			case discordgo.ActionsRow:
				walk(v.Components)
			case *discordgo.Button:
				if strings.HasPrefix(v.CustomID, pickPrefix) {
					out = append(out, strings.TrimPrefix(v.CustomID, pickPrefix))
				}
			case discordgo.Button:
				if strings.HasPrefix(v.CustomID, pickPrefix) {
					out = append(out, strings.TrimPrefix(v.CustomID, pickPrefix))
				}
			}
		}
	}
	walk(m.Components)
	return out
}

// handlePick answers a tap on a choice button.
func (h *Handler) handlePick(ctx context.Context, i *discordgo.Interaction, choice string) {
	if err := h.api.Respond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		slog.Warn("discord: acknowledge pick", "error", err)
	}
	user := interactionUser(i)
	if user == nil {
		return
	}
	userID, linked := h.addr.User(user.ID)
	if !linked {
		return
	}
	// Disable the buttons first, showing which was picked, so a second tap
	// (or a second person) can't answer the same question again.
	rows := choiceRow(pickedChoices(i.Message), true, choice)
	if h.buttons {
		rows = append(rows, h.replyButtons()...)
	}
	if _, err := h.api.Edit(&discordgo.MessageEdit{ID: i.Message.ID, Channel: i.ChannelID, Components: &rows}); err != nil {
		slog.Debug("discord: disable choice buttons", "error", err)
	}
	// The bot cannot post as the person, so it says what they picked — the
	// family sees the answer in the conversation — and that message stands in
	// for theirs.
	echo, err := h.api.Send(i.ChannelID, &discordgo.MessageSend{
		Content:         "**" + h.label(userID, user) + "** picked: " + choice,
		Reference:       &discordgo.MessageReference{MessageID: i.Message.ID, ChannelID: i.ChannelID},
		AllowedMentions: noPings(),
	})
	if err != nil {
		slog.Warn("discord: post pick", "error", err)
		return
	}
	question := i.Message
	if question.Author == nil {
		question.Author = &discordgo.User{ID: h.selfID, Bot: true}
	}
	h.HandleMessage(ctx, &discordgo.Message{
		ID: echo.ID, ChannelID: i.ChannelID, Content: choice, Timestamp: time.Now(),
		Author: user, ReferencedMessage: question,
	})
}
