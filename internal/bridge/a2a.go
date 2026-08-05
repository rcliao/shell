package bridge

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/rcliao/shell/internal/config"
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

// a2aDefaultMaxDepth caps consecutive agent→agent hops per chain before both
// must yield back to a human. Depth 1 = the first agent-to-peer message.
// Override per agent with agent.a2a_max_depth; see Bridge.SetA2AMaxDepth.
const a2aDefaultMaxDepth = 3

// A2AEventType is the shared-store event type for agent-to-agent group turns.
const A2AEventType = "a2a.message"

// a2aMarkerRe parses the synthetic prompt an A2A turn is delivered as:
//
//	[A2A from=<PeerName> depth=<N>] <the peer's actual message>
var a2aMarkerRe = regexp.MustCompile(`^\[A2A from=([^\]]+?) depth=(\d+)\]\s*`)

// A2APayload is the JSON body of an a2a.message event.
type A2APayload struct {
	ChatID int64 `json:"chat_id"`
	// ThreadID is the forum-topic thread the exchange lives in (0 = main).
	// Without it, peer replies to a topic conversation landed in the main
	// thread (owner report 7/18).
	ThreadID int64  `json:"thread_id"`
	From     string `json:"from"`  // human-facing peer name (for attribution)
	Text     string `json:"text"`  // the message the peer said in the group
	Depth    int    `json:"depth"` // hop count of THIS message
}

// parseA2AMarker extracts the incoming depth and peer name from a synthetic A2A
// prompt, returning the message reframed for the model as peer attribution.
// For an ordinary (non-A2A) message it returns depth 0 and the text unchanged.
func parseA2AMarker(userMsg string) (depth int, framed string, isA2A bool) {
	d, framed, _, ok := parseA2AMarkerFrom(userMsg)
	return d, framed, ok
}

// parseA2AMarkerFrom additionally reports WHICH peer sent the turn, so an
// in-flight exchange can be continued back to them (see maybeEnqueueA2A).
func parseA2AMarkerFrom(userMsg string) (depth int, framed string, from string, isA2A bool) {
	m := a2aMarkerRe.FindStringSubmatch(userMsg)
	if m == nil {
		return 0, userMsg, "", false
	}
	d, _ := strconv.Atoi(m[2])
	rest := userMsg[len(m[0]):]
	framed = fmt.Sprintf("[%s (your fellow agent) said this in the group — reply if you have something genuinely useful to add or a part of the task to take; otherwise [noop]]\n%s", m[1], rest)
	return d, framed, m[1], true
}

// peerByName resolves a peer by its human-facing name or any alias.
func (b *Bridge) peerByName(name string) *peerAddr {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return nil
	}
	for _, p := range b.peerAgents {
		if p.BotUsername == b.agentBotUsername {
			continue
		}
		for _, raw := range append([]string{p.Name}, p.Aliases...) {
			if strings.EqualFold(strings.TrimSpace(raw), want) {
				return &peerAddr{Name: p.Name, BotUsername: p.BotUsername, Alias: raw, Reason: "chain-continuation"}
			}
		}
	}
	return nil
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
func (b *Bridge) maybeEnqueueA2A(chatID, threadID int64, replyText string, incomingDepth int, fromPeer string) {
	if b.taskStore == nil || strings.TrimSpace(replyText) == "" {
		return
	}
	if chatID >= 0 { // groups have negative chat IDs; skip DMs and the system chat
		return
	}
	nextDepth := incomingDepth + 1
	if maxDepth := b.a2aMaxDepth(); nextDepth > maxDepth {
		slog.Info("a2a: depth cap reached, yielding to human", "chat_id", chatID, "depth", incomingDepth, "max_depth", maxDepth)
		return
	}
	peer := b.peerAddressedInReply(replyText)
	if peer == nil {
		// Chain continuation. Starting a NEW exchange still requires
		// explicitly addressing the peer — that is what keeps the agents from
		// striking up conversations nobody asked for. But once an exchange is
		// in flight, requiring every single reply to re-address the peer makes
		// a multi-turn agenda depend on each turn happening to phrase itself
		// the right way. It does not: the first scheduled agent sync died at
		// depth 1 of a four-item agenda because one reply omitted the name,
		// and nothing downstream could tell that from "we are finished".
		//
		// So an in-flight exchange continues back to whoever sent it — while
		// the reply is still ASKING something. A question invites an answer;
		// a plain statement is how a conversation ends. That terminates
		// naturally without a marker leaking into the family group, and it
		// keeps incidental peer chatter short instead of running to the cap.
		if incomingDepth > 0 && fromPeer != "" && invitesReply(replyText) {
			peer = b.peerByName(fromPeer)
		}
		if peer == nil {
			return
		}
	}
	payload, _ := json.Marshal(A2APayload{
		ChatID:   chatID,
		ThreadID: threadID,
		From:     b.selfDisplayName(),
		Text:     replyText,
		Depth:    nextDepth,
	})
	if err := b.taskStore.PublishEvent(peer.BotUsername, A2AEventType, string(payload)); err != nil {
		slog.Warn("a2a: failed to publish event", "to", peer.BotUsername, "error", err)
		return
	}
	slog.Info("a2a: handed off to peer", "from", b.agentBotUsername, "to", peer.BotUsername,
		"chat_id", chatID, "thread_id", threadID, "depth", nextDepth, "reason", peer.Reason)
}

// peerAddressedInReply returns the peer the reply is speaking TO, or nil.
// A hand-off is detected when the reply either (a) opens with the peer's name,
// (b) @mentions them, or (c) addresses them vocatively — the alias directly
// followed by vocative punctuation (comma/colon/?/!), which is how one agent
// actually calls the other ("...Hey Umbreon, you copy?"). A bare mid-sentence
// mention ("Umbreon usually handles plants") is deliberately NOT a hand-off.
func (b *Bridge) peerAddressedInReply(replyText string) *peerAddr {
	return DetectPeerAddress(replyText, b.peerAgents, b.agentBotUsername)
}

// PeerAddrMatch is one peer whose address DetectPeerAddress found (exported for
// the a2a-check diagnostic).
type PeerAddrMatch struct {
	Name        string
	BotUsername string
	Alias       string // the alias that matched
	Reason      string // mention | vocative | leading-question
}

// DetectPeerAddress is the pure detection used both by the live bridge and the
// `shell a2a-check` diagnostic. It returns the peer the reply is speaking TO
// (and why), or nil. A hand-off is: an @mention, an alias followed by vocative
// punctuation ("Umbreon, …" / "…哥哥，" / "…Umbreon!"), or the reply opening
// with the alias plus a question ("哥哥 你覺得…？"). A bare mid-sentence mention
// ("Umbreon usually handles plants") is deliberately NOT a hand-off.
func DetectPeerAddress(replyText string, peers []config.PeerAgent, selfBot string) *PeerAddrMatch {
	lower := strings.ToLower(replyText)
	leadLower := strings.ToLower(addressLeadStrip(replyText))
	hasQuestion := strings.ContainsAny(replyText, "?？")
	for _, p := range peers {
		if p.BotUsername == selfBot {
			continue
		}
		for _, raw := range append([]string{p.Name}, p.Aliases...) {
			a := strings.ToLower(strings.TrimSpace(raw))
			if a == "" {
				continue
			}
			var reason string
			switch {
			case strings.Contains(lower, "@"+a):
				reason = "mention"
			case vocativeAddressRe(a).MatchString(lower):
				reason = "vocative"
			case strings.HasPrefix(leadLower, a) && hasQuestion:
				reason = "leading-question"
			default:
				continue
			}
			return &PeerAddrMatch{Name: p.Name, BotUsername: p.BotUsername, Alias: raw, Reason: reason}
		}
	}
	return nil
}

// vocativeAddressRe matches the alias followed by vocative punctuation — a
// comma, colon, question, exclamation, or a dash (agents often write "Name —").
func vocativeAddressRe(alias string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s\p{P}])` + regexp.QuoteMeta(alias) + `\s*[,，、:：?？!！—–\-]`)
}

type peerAddr = PeerAddrMatch

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

// a2aMaxDepth returns the configured hop cap, falling back to the default when
// unset. Read through a method rather than the field directly so an
// unconfigured Bridge (tests, legacy single-agent setups) still gets a bound.
func (b *Bridge) a2aMaxDepth() int {
	if b.a2aMaxDepthCfg > 0 {
		return b.a2aMaxDepthCfg
	}
	return a2aDefaultMaxDepth
}

// SetA2AMaxDepth overrides the agent→agent hop cap for this agent. Values <= 0
// keep the default.
func (b *Bridge) SetA2AMaxDepth(n int) {
	b.a2aMaxDepthCfg = n
}

// turnPassRe matches handing the baton over WITHOUT asking a question.
//
// Added after the 2026-08-05 verification run: the chain died at the safety
// item because pika ended its turn with "換你念一遍你手上的" — "your turn, read
// out yours" — an unmistakable invitation that happens to contain no question
// mark. Requiring "?" was too narrow, and Chinese passes the baton with an
// imperative far more often than English does. Keep this list short and
// literal; a fuzzy match here would resurrect the chatter the address
// requirement exists to prevent.
var turnPassRe = regexp.MustCompile(`(?i)換你|輪到你|你先(講|說)|你呢|該你|\byour turn\b|\bover to you\b|\bwhat about you\b`)

// syncDoneRe matches an explicit close. Checked FIRST, so "that's everything —
// your turn next week" ends the exchange instead of extending it.
var syncDoneRe = regexp.MustCompile(`(?i)同步(結束|完成)|\bsync (is )?(finished|complete|done)\b|\bthat'?s everything\b`)

// invitesReply reports whether an in-flight turn is still handing the exchange
// back. A question invites an answer; so does explicitly passing the turn. A
// plain statement, or an explicit close, ends it.
func invitesReply(reply string) bool {
	if syncDoneRe.MatchString(reply) {
		return false
	}
	return strings.ContainsAny(reply, "?？") || turnPassRe.MatchString(reply)
}
