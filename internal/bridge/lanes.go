package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/route"
	"github.com/rcliao/shell/internal/store"
)

// Lanes (R1, docs/DESIGN-ROUTER-AND-SUGGESTIONS.md). In a chat listed in
// route.lane_chats, each real turn is routed BEFORE its session is chosen: a
// project lane gets its own Claude session (a negative "session thread"
// from lane_sessions) and that project's scoped [Project] block; the general
// lane keeps the chat's existing session. Delivery, transcript and message
// maps always use the real thread. Chats not listed are untouched.

// laneRouteTimeout bounds the synchronous router call on the turn path.
const laneRouteTimeout = 1500 * time.Millisecond

// SetLanes enables lanes. chats maps a chat to the chat whose active
// projects are its lanes (usually itself; a test chat can borrow another's).
func (b *Bridge) SetLanes(chats map[int64]int64, backend route.Backend) {
	b.laneChats = chats
	b.laneRouter = backend
}

// SetLanesAll turns lanes on for every chat (route.lane_chats {"*": …}),
// each chat using its own projects; explicit entries still win.
func (b *Bridge) SetLanesAll(all bool) { b.lanesAll = all }

// laneForTurn routes one turn. ok is false when lanes are off for the chat;
// otherwise it returns the session thread to use and the context block for
// the lane ("" for general).
func (b *Bridge) laneForTurn(ctx context.Context, chatID, threadID int64, text string) (sessThread int64, block string, ok bool) {
	candChat, on := b.laneChats[chatID]
	if !on && b.lanesAll {
		candChat, on = chatID, true
	}
	if !on || b.store == nil || chatID == 0 {
		return threadID, "", false
	}
	projects, err := b.store.ListProjects(candChat)
	if err != nil {
		slog.Warn("lanes: project list failed", "chat_id", chatID, "error", err)
		return threadID, "", true
	}
	// A project's own forum topic already decides its context by thread
	// (the scoped [Project] block): routing there could only move a message
	// out of its own project. Leave bound topics exactly as they were.
	if threadID != 0 {
		for _, p := range projects {
			if p.Status == "active" && p.MessageThreadID == threadID && candChat == chatID {
				return threadID, "", false
			}
		}
	}
	var cands []route.Candidate
	bySlug := map[string]store.Project{}
	for _, p := range projects {
		if p.Status == "active" {
			cands = append(cands, route.Candidate{Lane: p.Slug, Title: p.Title, Desc: p.Instructions})
			bySlug[p.Slug] = p
		}
	}
	prev, _ := b.store.LastRouteLane("lane", laneBackendName(b.laneRouter), chatID, threadID)
	choice := route.Choice{Lane: prev}
	if choice.Lane == "" || b.laneRouter == nil {
		// No router (no key): everything is general, never a stuck lane.
		choice = route.Choice{Lane: route.General, Confidence: 1}
	}
	if len(cands) > 0 && b.laneRouter != nil {
		rctx, cancel := context.WithTimeout(ctx, laneRouteTimeout)
		c, err := b.laneRouter.Choose(rctx, route.Input{ChatKind: chatKindOf(chatID), ThreadID: threadID, Text: text, Candidates: cands})
		cancel()
		if err != nil {
			// No answer in time: stay where the thread was. A confident
			// "same lane" choice so the sticky rule does not second-guess it.
			slog.Warn("lanes: router failed, keeping previous lane", "chat_id", chatID, "lane", choice.Lane, "error", err)
			choice.Confidence = 1
		} else {
			choice = c
		}
	}
	lane, sticky := route.Decide(prev, choice, b.stickyThreshold())
	if _, known := bySlug[lane]; !known {
		lane = route.General // a project archived since, or a stale previous lane
	}
	if err := b.store.LogRouteDecision(store.RouteDecision{Source: "lane", ChatID: chatID, ThreadID: threadID,
		MsgID: int64(telegramMsgIDFrom(ctx)), MsgAt: time.Now(), TextHash: store.TextHash(text),
		Backend: laneBackendName(b.laneRouter), LanePrev: prev, Choice: choice.Lane, Confidence: choice.Confidence,
		Lane: lane, Sticky: sticky, LatencyMS: choice.Latency.Milliseconds()}); err != nil {
		slog.Warn("lanes: decision log failed", "error", err)
	}
	sessThread, err = b.store.LaneSessionThread(chatID, threadID, lane)
	if err != nil {
		slog.Warn("lanes: session allocation failed, using the chat's session", "chat_id", chatID, "lane", lane, "error", err)
		return threadID, "", true
	}
	if p, isProject := bySlug[lane]; isProject {
		block = b.laneProjectBlock(p, projects)
	}
	slog.Info("lanes: routed", "chat_id", chatID, "thread_id", threadID, "lane", lane, "session_thread", sessThread,
		"confidence", choice.Confidence, "sticky", sticky)
	return sessThread, block, true
}

func laneBackendName(b route.Backend) string {
	if b == nil {
		return "none"
	}
	return b.Name()
}

func chatKindOf(chatID int64) string {
	if chatID < 0 {
		return "group"
	}
	return "dm"
}

// realThread maps a lane's session thread back to the real thread (identity
// for everything else, and when there is no store).
func (b *Bridge) realThread(chatID, threadID int64) int64 {
	if threadID >= 0 || b.store == nil {
		return threadID
	}
	return b.store.RealThread(chatID, threadID)
}

const (
	// laneRecentLimit and laneRecentWindow bound the "missed in other lanes"
	// block: enough to carry a conversation across a lane switch, not a
	// transcript.
	laneRecentLimit  = 8
	laneRecentWindow = 12 * time.Hour
	laneRecentRunes  = 300
)

// laneRecentBlock is what this lane's session missed: the latest messages of
// the same real thread said in its other sessions (the general one, other
// lanes) since this session last spoke. A brand-new lane gets the thread's
// last few messages; a lane switched back into gets what happened meanwhile.
// "" when nothing was missed.
func (b *Bridge) laneRecentBlock(chatID, realThread, sessThread int64) string {
	if b.store == nil {
		return ""
	}
	msgs, err := b.store.RecentOutsideSession(chatID, realThread, sessThread, laneRecentLimit, laneRecentWindow)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Recent in this chat, outside this lane — oldest first; for continuity, not to be answered]\n")
	for _, m := range msgs {
		who := "them"
		if m.Role == "assistant" {
			who = "you"
		}
		t := []rune(strings.Join(strings.Fields(m.Text), " "))
		if len(t) > laneRecentRunes {
			t = append(t[:laneRecentRunes], '…')
		}
		fmt.Fprintf(&sb, "[%s %s]: %s\n", m.At.Local().Format("15:04"), who, string(t))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// noteLaneTurn remembers which session answered a Telegram message, so the
// message map (reactions, regenerate) points at the lane's session.
func (b *Bridge) noteLaneTurn(chatID int64, msgID int, sessionID int64) {
	if msgID != 0 {
		b.laneTurnSess.Store(laneTurnKey(chatID, msgID), sessionID)
	}
}

func laneTurnKey(chatID int64, msgID int) string {
	return fmt.Sprintf("%d/%d", chatID, msgID)
}
