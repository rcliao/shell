package daemon

import (
	"context"
	"log/slog"
	"strings"
	"time"

	shellbrowser "github.com/rcliao/shell-browser"
	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/browserhandoff"
	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/store"
)

// browserHandoffSender names the synthetic turn that resumes an agent after a
// handoff. It must not be a system sender (bridge.isSystemSender): the resume
// is a real turn whose reply goes to the chat.
const browserHandoffSender = "browser-handoff"

// newBrowserHandoffs builds the agent's handoff manager: live views published
// with tailscale serve, the link posted with a button on whichever platform
// the chat lives on, and the resume turn delivered like an A2A peer turn.
func newBrowserHandoffs(bc config.BrowserConfig, agentName, sessionsDir string, st *store.Store, bot outbound, br *bridge.Bridge) *browserhandoff.Manager {
	pol, err := shellbrowser.LoadPolicy(shellbrowser.PolicyPath())
	if err != nil {
		slog.Warn("browser handoffs: policy file unreadable, using the default policy", "error", err)
		pol = shellbrowser.DefaultPolicy()
	}
	minutes := func(v, def int) time.Duration {
		if v <= 0 {
			v = def
		}
		return time.Duration(v) * time.Minute
	}
	return browserhandoff.New(browserhandoff.Config{
		AgentName:  agentName,
		DefaultTTL: minutes(bc.HandoffTTLMin, 10),
		MaxTTL:     minutes(bc.MaxTTLMin, 60),
		Store:      st,
		Publisher:  &browserhandoff.Tailscale{Bin: bc.TailscaleBin},
		Sessions:   &browserhandoff.ChromeSessions{Root: sessionsDir, Policy: pol},
		Notify: func(chatID, threadID int64, text, label, link string) error {
			return bot.SendTextButtons(chatID, threadID, text, []bridge.LinkButton{{Label: label, URL: link}})
		},
		Resume: func(chatID, threadID int64, prompt string) {
			resp, err := syntheticTurn(context.Background(), br, chatID, threadID, prompt, browserHandoffSender)
			if err != nil {
				slog.Error("browser handoff: resume turn failed", "chat_id", chatID, "error", err)
				return
			}
			if strings.TrimSpace(resp.Text) == "" && len(resp.Photos) == 0 {
				return
			}
			for _, photo := range resp.Photos {
				bot.SendPhoto(chatID, threadID, photo.Data, photo.Caption)
			}
			if strings.TrimSpace(resp.Text) != "" {
				bot.SendText(chatID, threadID, resp.Text)
			}
		},
	})
}
