package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
	"github.com/rcliao/shell/internal/bridge"
)

// linkButtonMarkup renders one URL button per row, and returns nil (omit
// reply_markup entirely) for an empty slice — the "behaves exactly like the
// buttonless method" contract.
func TestLinkButtonMarkup(t *testing.T) {
	if got := linkButtonMarkup(nil); got != nil {
		t.Errorf("nil buttons should produce nil markup, got %+v", got)
	}
	if got := linkButtonMarkup([]bridge.LinkButton{}); got != nil {
		t.Errorf("empty buttons should produce nil markup, got %+v", got)
	}

	buttons := []bridge.LinkButton{
		{Label: "📄 🏠 Housing Search", URL: "https://notion.so/aaaa1111222243338444555566667777"},
		{Label: "📄 開啟文件", URL: "https://notion.so/bbbb1111222243338444555566667777"},
	}
	markup, ok := linkButtonMarkup(buttons).(*models.InlineKeyboardMarkup)
	if !ok {
		t.Fatalf("markup type = %T, want *models.InlineKeyboardMarkup", linkButtonMarkup(buttons))
	}
	if len(markup.InlineKeyboard) != 2 {
		t.Fatalf("rows = %d, want one button per row", len(markup.InlineKeyboard))
	}
	for i, row := range markup.InlineKeyboard {
		if len(row) != 1 {
			t.Fatalf("row %d has %d buttons, want 1", i, len(row))
		}
		if row[0].Text != buttons[i].Label || row[0].URL != buttons[i].URL {
			t.Errorf("row %d = %+v, want label %q url %q (order preserved)", i, row[0], buttons[i].Label, buttons[i].URL)
		}
		if row[0].CallbackData != "" {
			t.Errorf("row %d carries callback data — URL buttons only", i)
		}
	}
}
