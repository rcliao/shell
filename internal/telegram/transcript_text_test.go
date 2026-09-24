package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestTranscriptText(t *testing.T) {
	cases := []struct {
		msg  models.Message
		want string
	}{
		{models.Message{Text: "  hi  "}, "hi"},
		{models.Message{Caption: "look", Photo: []models.PhotoSize{{}}}, "(photo) look"},
		{models.Message{Photo: []models.PhotoSize{{}}}, "(photo)"},
		{models.Message{Sticker: &models.Sticker{Emoji: "😀"}}, "(sticker 😀)"},
		{models.Message{Voice: &models.Voice{}}, "(voice message)"},
		{models.Message{}, ""},
	}
	for _, c := range cases {
		if got := transcriptText(&c.msg); got != c.want {
			t.Errorf("transcriptText = %q, want %q", got, c.want)
		}
	}
}
