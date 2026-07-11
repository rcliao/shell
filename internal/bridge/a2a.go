package bridge

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
)

// Agent-to-agent (A2A) group conversation.
//
// Telegram never delivers one bot's message to another bot, so peer agents
// cannot hear each other directly. Instead: when an agent posts a group reply
// addressed to its peer, we publish an "a2a.message" event to the peer via the
// shared task store's event table; the peer's daemon poll picks it up and runs
// a normal group turn (posting its reply to the group). A depth counter carried
// in the message bounds the exchange so they don't chatter forever.
//
// Symmetric by design — either agent may address the other; there is no leader.
// Reset is automatic: a human-triggered reply starts a fresh chain at depth 0,
// so every human message renews the budget.

// a2aMaxDepth caps consecutive agent→agent hops per chain before both must
// yield back to a human. Depth 1 = the first agent-to-peer message.
const a2aMaxDepth = 3

// A2AEventType is the shared-store event type for agent-to-agent group turns.
const A2AEventType = "a2a.message"

// a2aMarkerRe parses the synthetic prompt an A2A turn is delivered as:
//
//	[A2A from=<PeerName> depth=<N>] <the peer's actual message>
var a2aMarkerRe = regexp.MustCompile(`^\[A2A from=([^\]]+?) depth=(\d+)\]\s*`)

// A2APayload is the JSON body of an a2a.message event.
type A2APayload struct {
	ChatID int64  `json:"chat_id"`
	From   string `json:"from"`  // human-facing peer name (for attribution)
	Text   string `json:"text"`  // the message the peer said in the group
	Depth  int    `json:"depth"` // hop count of THIS message
}

// parseA2AMarker extracts the incoming depth and peer name from a synthetic A2A
// prompt, returning the message reframed for the model as peer attribution.
// For an ordinary (non-A2A) message it returns depth 0 and the text unchanged.
func parseA2AMarker(userMsg string) (depth int, framed string, isA2A bool) {
	m := a2aMarkerRe.FindStringSubmatch(userMsg)
	if m == nil {
		return 0, userMsg, false
	}
	d, _ := strconv.Atoi(m[2])
	rest := userMsg[len(m[0]):]
	framed = fmt.Sprintf("[%s (your fellow agent) said this in the group — reply if you have something genuinely useful to add or a part of the task to take; otherwise [noop]]\n%s", m[1], rest)
	return d, framed, true
}

// A2ADeliveryPrompt builds the synthetic prompt used to deliver an A2A turn
// to the peer (consumed by the daemon poll, parsed back by parseA2AMarker).
func A2ADeliveryPrompt(fromName string, depth int, text string) string {
	return fmt.Sprintf("[A2A from=%s depth=%d] %s", fromName, depth, text)
}

// maybeEnqueueA2A publishes an A2A turn to a peer when the just-produced group
// reply is addressed to that peer and the chain is still under the depth cap.
// incomingDepth is the depth of the message THIS turn was answering (0 for a
// human turn). No-op unless the chat is a group, the reply is non-empty, a peer
// is addressed, and a task store is configured.
func (b *Bridge) maybeEnqueueA2A(chatID int64, replyText string, incomingDepth int) {
	if b.taskStore == nil || strings.TrimSpace(replyText) == "" {
		return
	}
	if chatID >= 0 { // groups have negative chat IDs; skip DMs and the system chat
		return
	}
	nextDepth := incomingDepth + 1
	if nextDepth > a2aMaxDepth {
		slog.Info("a2a: depth cap reached, yielding to human", "chat_id", chatID, "depth", incomingDepth)
		return
	}
	peer := b.peerAddressedInReply(replyText)
	if peer == nil {
		return
	}
	payload, _ := json.Marshal(A2APayload{
		ChatID: chatID,
		From:   b.selfDisplayName(),
		Text:   replyText,
		Depth:  nextDepth,
	})
	if err := b.taskStore.PublishEvent(peer.BotUsername, A2AEventType, string(payload)); err != nil {
		slog.Warn("a2a: failed to publish event", "to", peer.BotUsername, "error", err)
		return
	}
	slog.Info("a2a: handed off to peer", "from", b.agentBotUsername, "to", peer.BotUsername,
		"chat_id", chatID, "depth", nextDepth)
}

// peerAddressedInReply returns the peer the reply is speaking TO, or nil.
// A hand-off is detected when the reply either (a) opens with the peer's name,
// (b) @mentions them, or (c) addresses them vocatively — the alias directly
// followed by vocative punctuation (comma/colon/?/!), which is how one agent
// actually calls the other ("...Hey Umbreon, you copy?"). A bare mid-sentence
// mention ("Umbreon usually handles plants") is deliberately NOT a hand-off.
func (b *Bridge) peerAddressedInReply(replyText string) *peerAddr {
	lower := strings.ToLower(replyText)
	leadLower := strings.ToLower(addressLeadStrip(replyText))
	hasQuestion := strings.ContainsAny(replyText, "?？")
	for _, p := range b.peerAgents {
		if p.BotUsername == b.agentBotUsername {
			continue
		}
		for _, raw := range append([]string{p.Name}, p.Aliases...) {
			a := strings.ToLower(strings.TrimSpace(raw))
			if a == "" {
				continue
			}
			switch {
			case strings.Contains(lower, "@"+a): // @mention
			case vocativeAddressRe(a).MatchString(lower): // "…umbreon, …" vocative
			case strings.HasPrefix(leadLower, a) && hasQuestion: // "哥哥 你覺得…？"
			default:
				continue
			}
			return &peerAddr{Name: p.Name, BotUsername: p.BotUsername}
		}
	}
	return nil
}

// vocativeAddressRe matches the alias directly followed by vocative punctuation.
func vocativeAddressRe(alias string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s\p{P}])` + regexp.QuoteMeta(alias) + `\s*[,，、:：?？!！]`)
}

type peerAddr struct {
	Name        string
	BotUsername string
}

// selfDisplayName is this agent's human-facing name for peer attribution.
func (b *Bridge) selfDisplayName() string {
	if b.agentIdentityName != "" {
		return b.agentIdentityName
	}
	return b.agentBotUsername
}

var a2aLeadRe = regexp.MustCompile(`^[\s\p{P}\p{S}]+`)

func addressLeadStrip(s string) string {
	return a2aLeadRe.ReplaceAllString(s, "")
}
