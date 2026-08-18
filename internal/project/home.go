package project

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// The pinned 📋 Projects home (P2, docs/PLAN-PROJECT-WORKSPACE.md "Project
// home"): one bot-edited message per chat, pinned, listing the chat's active
// projects. The message id lives in chat_pins; refreshes EDIT that message in
// place rather than sending a new one, so the chat is not spammed every time
// a doc write lands.

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
// Declared here (consumer side) so this package needs no bridge import.
type HomeTransport interface {
	SendMessageID(chatID, threadID int64, text string) (int, error)
	EditMessage(chatID int64, messageID int, text string) error
	PinMessage(chatID int64, messageID int, silent bool) error
	UnpinMessage(chatID int64, messageID int) error
}

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
		// Needs-you hint goes here in P4 (comment loop lands the signal).
		if p.DocPath != "" {
			sb.WriteString(" 📄")
		}
		lines = append(lines, sb.String())
	}
	if len(lines) == 0 {
		return "📋 Projects\n(no active projects)"
	}
	return "📋 Projects\n" + strings.Join(lines, "\n")
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

	text, err := h.render(chatID)
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
		err := h.transport.EditMessage(chatID, pin.ProjectsMsgID, text)
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
	h.sendAndPin(chatID, 0, text)
}

// Repin sends a FRESH home message into the given thread, pins it, records
// it, and best-effort unpins the previous one. This is the /projects command:
// an explicit human ask bypasses the debounce.
func (h *Home) Repin(chatID, threadID int64) error {
	if h == nil {
		return fmt.Errorf("projects home not wired")
	}
	text, err := h.render(chatID)
	if err != nil {
		return err
	}
	old, err := h.store.GetChatPin(chatID)
	if err != nil {
		slog.Warn("projects home: pin lookup failed", "chat_id", chatID, "error", err)
	}
	if !h.sendAndPin(chatID, threadID, text) {
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

func (h *Home) render(chatID int64) (string, error) {
	projects, err := h.store.ListProjects(chatID)
	if err != nil {
		return "", err
	}
	return RenderHome(projects), nil
}

// sendAndPin sends the home text, pins it silently, and records the id.
// Reports whether the send itself succeeded — a failed PIN still leaves a
// correct row pointing at a real message, so it only warns.
func (h *Home) sendAndPin(chatID, threadID int64, text string) bool {
	msgID, err := h.transport.SendMessageID(chatID, threadID, text)
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
