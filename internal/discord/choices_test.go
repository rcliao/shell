package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestExtractChoices(t *testing.T) {
	rest, cs := extractChoices("Dinner tonight?\n```choices\n[\"Tacos\", \"Ramen\", \"Tacos\", \" \"]\n```")
	if rest != "Dinner tonight?" || strings.Join(cs, "|") != "Tacos|Ramen" {
		t.Fatalf("rest=%q choices=%q", rest, cs)
	}
	for _, bad := range []string{
		"```choices\nnot json\n```",
		"```choices\n[\"a\",\"b\",\"c\",\"d\",\"e\",\"f\"]\n```", // six: too many
		"```choices\n[]\n```",
	} {
		if rest, cs := extractChoices(bad); cs != nil || rest != strings.TrimSpace(bad) {
			t.Errorf("%q should stay as written, got choices=%q", bad, cs)
		}
	}
}

const question = "Dinner tonight?\n```choices\n[\"Tacos\", \"Ramen\"]\n```"

func TestChoicesBecomeButtonsAndATapAnswers(t *testing.T) {
	h := newHarness(t, question)
	h.bot.handler.choices = true
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000100", dmChan, linkedUser, "what should we eat"))
	q := h.api.sends[0].id
	if got := h.api.edits[q]; got != "Dinner tonight?" {
		t.Fatalf("question text = %q (the block must become buttons)", got)
	}
	if h.api.compsOn[q] != 1 {
		t.Fatalf("want one row of choice buttons, got %d", h.api.compsOn[q])
	}

	h.agent.reply = "Tacos it is!"
	row := choiceRow([]string{"Tacos", "Ramen"}, false, "")
	h.bot.handler.HandleInteraction(context.Background(), &discordgo.Interaction{
		Type: discordgo.InteractionMessageComponent, ChannelID: dmChan,
		Message: &discordgo.Message{ID: q, Components: row, Author: &discordgo.User{ID: selfBot, Bot: true}, Content: "Dinner tonight?"},
		User:    &discordgo.User{ID: linkedUser},
		Data:    discordgo.MessageComponentInteractionData{CustomID: pickPrefix + "Tacos"},
	})
	var echo string
	for _, s := range h.api.sends {
		if strings.Contains(s.content, "picked: Tacos") {
			echo = s.content
		}
	}
	if !strings.Contains(echo, "The Owner") {
		t.Fatalf("the pick should be posted with who picked it, got %q", echo)
	}
	if h.agent.turnCount() != 2 {
		t.Fatalf("a tap is a new turn: turns=%d", h.agent.turnCount())
	}
	turn := h.agent.turns[1].Text
	if !strings.Contains(turn, "Tacos") || !strings.Contains(turn, `[Replying to your earlier message: "Dinner tonight?"]`) {
		t.Fatalf("the agent should get the answer with its question:\n%s", turn)
	}
}

func TestChoicesOffLeavesTheBlock(t *testing.T) {
	h := newHarness(t, question)
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000101", dmChan, linkedUser, "what should we eat"))
	if got := h.api.edits[h.api.sends[0].id]; !strings.Contains(got, "```choices") {
		t.Fatalf("with the experiment off the text is as written, got %q", got)
	}
}

// A scheduled or relayed message goes out through SendText, not a live
// turn; its choices block still becomes buttons instead of raw text.
func TestOutboundSendTurnsChoicesIntoButtons(t *testing.T) {
	h := newHarness(t, "")
	h.bot.handler.choices = true
	h.bot.SendText(-100200300, 0, "Set a reminder?\n\n```choices\n[\"Yes\", \"Not now\"]\n```")
	if len(h.api.sends) != 1 {
		t.Fatalf("sends = %+v", h.api.sends)
	}
	got := h.api.sends[0]
	if strings.Contains(got.content, "```choices") || got.content != "Set a reminder?" {
		t.Fatalf("choices block leaked into the text: %q", got.content)
	}
	if got.buttons != 1 {
		t.Fatalf("want one button row, got %d", got.buttons)
	}

	// With the experiment off the text is left as written.
	h2 := newHarness(t, "")
	h2.bot.SendText(-100200300, 0, "Q?\n```choices\n[\"A\"]\n```")
	if !strings.Contains(h2.api.sends[0].content, "```choices") || h2.api.sends[0].buttons != 0 {
		t.Fatalf("experiment off: %+v", h2.api.sends[0])
	}
}
