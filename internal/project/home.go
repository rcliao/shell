package project

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/store"
)

// The pinned 📋 Projects home (P2, docs/PLAN-PROJECT-WORKSPACE.md "Project
// home"): one bot-edited message per chat, pinned, listing the chat's active
// projects. The message id lives in chat_pins; refreshes EDIT that message in
// place rather than sending a new one, so the chat is not spammed every time
// a doc write lands.

// recentCommentWindow bounds the 💬 marker: a project whose Notion comment
// loop processed a discussion this recently is marked as having an active
// conversation — the cheap needs-you precursor (Wave D).
const recentCommentWindow = 24 * time.Hour

// homeDebounce bounds edits to one per chat per window. EditMessage retries
// flood errors but does not debounce (Wave A note) — a burst of doc writes
// must not turn into a burst of Telegram edits.
const homeDebounce = 60 * time.Second

// HomeStore is the slice of the store the home renderer needs.
type HomeStore interface {
	ListProjects(chatID int64) ([]store.Project, error)
	GetChatPin(chatID int64) (*store.ChatPin, error)
	SetChatPin(chatID int64, msgID int) error
}

// HomeTransport is the slice of the bridge Transport the home renderer needs.
// Declared here (consumer side); the bridge import is for the LinkButton
// value type only. The home always sends through the button variants — a nil
// slice degrades to a plain send — so the buttonless methods are not needed.
type HomeTransport interface {
	SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error)
	EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error
	PinMessage(chatID int64, messageID int, silent bool) error
	UnpinMessage(chatID int64, messageID int) error
}

// Button-row limits: one tappable 📄 button per doc-bearing project, capped
// so a chat with many projects does not grow a keyboard taller than the
// message. Projects past the cap keep their list line; only the button is
// dropped. Titles are truncated so a button stays one tap-sized line.
const (
	maxHomeButtons      = 6
	maxButtonTitleRunes = 24
)

// Home maintains the pinned projects message for each chat.
type Home struct {
	store     HomeStore
	transport HomeTransport

	mu       sync.Mutex
	lastEdit map[int64]time.Time // chat_id → last edit, for the debounce
}

// NewHome creates a Home over the given store and transport.
func NewHome(st HomeStore, tr HomeTransport) *Home {
	return &Home{store: st, transport: tr, lastEdit: map[int64]time.Time{}}
}

// RenderHome renders the projects-home message body: a header and one line
// per ACTIVE project. Paused and archived projects leave the pinned list
// (plan §Lifecycle: archive removes finally, pause removes revivably).
// Plain text on purpose — the transport's send/edit path owns escaping.
func RenderHome(projects []store.Project) string {
	var lines []string
	for _, p := range projects {
		if p.Status != "active" {
			continue
		}
		var sb strings.Builder
		if p.Emoji != "" {
			sb.WriteString(p.Emoji)
			sb.WriteString(" ")
		}
		sb.WriteString(p.Title)
		sb.WriteString(" — ")
		sb.WriteString(lastActivity(p).Format("2006-01-02"))
		if p.DocPath != "" {
			sb.WriteString(" 📄")
		}
		// Comment-loop activity marker (Wave D): a discussion processed in
		// the last day means a live conversation on the project's page.
		if hasRecentComment(p, time.Now()) {
			sb.WriteString(" 💬")
		}
		lines = append(lines, sb.String())
	}
	if len(lines) == 0 {
		return "📋 Projects\n(no active projects)"
	}
	return "📋 Projects\n" + strings.Join(lines, "\n")
}

// HomeButtons renders one inline URL button per active project whose Notion
// page URL is derivable (export_kind=notion plus a block map WE rendered —
// same rule as the RPC get), in list order, capped at maxHomeButtons.
func HomeButtons(projects []store.Project) []bridge.LinkButton {
	var buttons []bridge.LinkButton
	for _, p := range projects {
		if p.Status != "active" {
			continue
		}
		url := NotionPageURL(p)
		if url == "" {
			continue
		}
		if len(buttons) == maxHomeButtons {
			break // the rest keep their list line, just no button
		}
		buttons = append(buttons, bridge.LinkButton{Label: buttonLabel(p), URL: url})
	}
	return buttons
}

// buttonLabel renders "📄 <emoji> <title>" with the title truncated to a
// tap-sized length.
func buttonLabel(p store.Project) string {
	title := []rune(p.Title)
	if len(title) > maxButtonTitleRunes {
		title = append(title[:maxButtonTitleRunes-1], '…')
	}
	label := "📄 "
	if p.Emoji != "" {
		label += p.Emoji + " "
	}
	return label + string(title)
}

// hasRecentComment reports whether any handled Notion discussion landed
// within the recent-comment window.
func hasRecentComment(p store.Project, now time.Time) bool {
	for _, h := range store.ParseHandledDiscussions(p.HandledDiscussions) {
		if !h.At.IsZero() && now.Sub(h.At) < recentCommentWindow {
			return true
		}
	}
	return false
}

// lastActivity picks the most recent thing that happened to a project: a
// research pass, a human touch, or any row update.
func lastActivity(p store.Project) time.Time {
	t := p.UpdatedAt
	if p.LastResearchAt != nil && p.LastResearchAt.After(t) {
		t = *p.LastResearchAt
	}
	if p.LastHumanActivityAt != nil && p.LastHumanActivityAt.After(t) {
		t = *p.LastHumanActivityAt
	}
	return t
}

// Refresh re-renders the chat's home and applies it: edit in place when a
// pinned message exists, otherwise send + pin (silently) + record the id.
// Debounced to one edit per chat per minute — a skipped refresh is caught up
// by the next trigger. Errors are logged, never returned: the home is a
// convenience surface and must not fail the operation that triggered it.
//
// A fresh send targets thread 0. The home is chat-level (chat_pins is keyed
// by chat alone); /projects re-pins it into a specific topic when wanted.
func (h *Home) Refresh(chatID int64) {
	if h == nil || chatID == 0 {
		return
	}
	h.mu.Lock()
	if last, ok := h.lastEdit[chatID]; ok && time.Since(last) < homeDebounce {
		h.mu.Unlock()
		slog.Debug("projects home: refresh debounced", "chat_id", chatID)
		return
	}
	h.lastEdit[chatID] = time.Now()
	h.mu.Unlock()

	text, buttons, err := h.render(chatID)
	if err != nil {
		slog.Warn("projects home: render failed", "chat_id", chatID, "error", err)
		return
	}

	pin, err := h.store.GetChatPin(chatID)
	if err != nil {
		slog.Warn("projects home: pin lookup failed", "chat_id", chatID, "error", err)
		return
	}
	if pin != nil {
		err := h.transport.EditMessageButtons(chatID, pin.ProjectsMsgID, text, buttons)
		if err == nil {
			return
		}
		if !messageGone(err) {
			slog.Warn("projects home: edit failed", "chat_id", chatID, "msg_id", pin.ProjectsMsgID, "error", err)
			return
		}
		// The pinned message was deleted out from under us — fall through and
		// mint a fresh one.
		slog.Info("projects home: pinned message gone, re-sending", "chat_id", chatID, "msg_id", pin.ProjectsMsgID)
	}
	h.sendAndPin(chatID, 0, text, buttons)
}

// Repin sends a FRESH home message into the given thread, pins it, records
// it, and best-effort unpins the previous one. This is the /projects command:
// an explicit human ask bypasses the debounce.
func (h *Home) Repin(chatID, threadID int64) error {
	if h == nil {
		return fmt.Errorf("projects home not wired")
	}
	text, buttons, err := h.render(chatID)
	if err != nil {
		return err
	}
	old, err := h.store.GetChatPin(chatID)
	if err != nil {
		slog.Warn("projects home: pin lookup failed", "chat_id", chatID, "error", err)
	}
	if !h.sendAndPin(chatID, threadID, text, buttons) {
		return fmt.Errorf("could not send projects list")
	}
	if old != nil {
		// Best-effort: the old message may already be deleted or unpinned.
		if err := h.transport.UnpinMessage(chatID, old.ProjectsMsgID); err != nil {
			slog.Debug("projects home: old unpin failed", "chat_id", chatID, "msg_id", old.ProjectsMsgID, "error", err)
		}
	}
	h.mu.Lock()
	h.lastEdit[chatID] = time.Now()
	h.mu.Unlock()
	return nil
}

func (h *Home) render(chatID int64) (string, []bridge.LinkButton, error) {
	projects, err := h.store.ListProjects(chatID)
	if err != nil {
		return "", nil, err
	}
	return RenderHome(projects), HomeButtons(projects), nil
}

// sendAndPin sends the home text, pins it silently, and records the id.
// Reports whether the send itself succeeded — a failed PIN still leaves a
// correct row pointing at a real message, so it only warns.
func (h *Home) sendAndPin(chatID, threadID int64, text string, buttons []bridge.LinkButton) bool {
	msgID, err := h.transport.SendMessageIDButtons(chatID, threadID, text, buttons)
	if err != nil {
		slog.Warn("projects home: send failed", "chat_id", chatID, "error", err)
		return false
	}
	if err := h.transport.PinMessage(chatID, msgID, true); err != nil {
		slog.Warn("projects home: pin failed", "chat_id", chatID, "msg_id", msgID, "error", err)
	}
	if err := h.store.SetChatPin(chatID, msgID); err != nil {
		slog.Warn("projects home: pin record failed", "chat_id", chatID, "msg_id", msgID, "error", err)
	}
	return true
}

// messageGone reports whether an edit failed because the target message no
// longer exists (deleted by a user or by Telegram retention), which is the
// one edit failure that should trigger a fresh send instead of a retry.
func messageGone(err error) bool {
	s := err.Error()
	return strings.Contains(s, "message to edit not found") ||
		strings.Contains(s, "MESSAGE_ID_INVALID")
}
