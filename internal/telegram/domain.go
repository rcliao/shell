package telegram

import (
	"regexp"
	"strings"
)

// Role-based group routing.
//
// Every group message reaches BOTH agents. To make the right number of agents
// answer — deterministically, without the two daemons coordinating — both run
// this identical classifier and decide independently. A message is CLEARLY one
// domain (→ only that agent's owner answers), or AMBIGUOUS (→ both may answer,
// so neither vanishes from the chat). Assignment lives in config
// (agent.group_domain): pikamini owns "practical", umbreonmini "companionship".
// A named/@mention message overrides routing (the addressed agent answers).

const (
	DomainPractical     = "practical"     // default: task/info/product/plants/food — practical owner answers
	DomainCompanionship = "companionship" // feelings/mood/comfort — companionship owner answers
	DomainSocial        = "social"        // greetings/laughter/reactions/plural address — BOTH stay present
)

// companionshipEN / companionshipCJK: emotional / connection-seeking cues.
var companionshipEN = regexp.MustCompile(`(?i)\b(feel|feeling|feelings|tired|exhausted|sad|lonely|miss you|love you|stressed|anxious|worried|overwhelmed|comfort|cheer me|vent|upset|cry|crying|hug|proud of|how are you|are you ok|are you okay|talk to you|chat with you)\b`)
var companionshipCJK = []string{
	"累", "好累", "心累", "難過", "傷心", "心情", "想你", "想妳", "陪我", "陪陪",
	"孤單", "寂寞", "壓力", "焦慮", "擔心", "抱抱", "想哭", "委屈", "難受",
	"心疼", "愛你", "愛妳", "你好嗎", "還好嗎", "聊聊", "傾訴", "撐不住",
	"煩死", "好煩", "情緒", "陪伴",
}

// socialEN / socialCJK / plural address: greetings, laughter, reactions, and
// messages aimed at BOTH agents. These keep both present in the chat's
// connective tissue rather than silencing the non-practical owner.
var socialEN = regexp.MustCompile(`(?i)^\s*(hi|hey|hello|yo|morning|good morning|good night|gnight|lol|lmao|haha+|hehe+|wow|omg|aww+|ah+|yay|ok babies|hi babies|hey babies)[\s!.~]*$|(\bbabies\b|\byou (both|two)\b)`)
var socialCJK = []string{
	"哈哈", "呵呵", "嘿嘿", "嗨", "哈囉", "早安", "晚安", "大家", "你們", "妳們",
	"寶貝們", "兩隻", "哇", "天啊", "太好笑", "笑死", "😂", "🤣",
}

// ClassifyGroupDomain returns companionship | social | practical (default).
// Order: emotional first (a feeling takes priority even over practical nouns —
// "今天煮飯好累" needs comfort); then social/plural-address (both stay present);
// else practical — the default, so pika reliably owns the task/info firehose
// (product/plant/food/how-to) without needing every keyword enumerated.
func ClassifyGroupDomain(text string) string {
	if companionshipEN.MatchString(text) {
		return DomainCompanionship
	}
	for _, s := range companionshipCJK {
		if strings.Contains(text, s) {
			return DomainCompanionship
		}
	}
	if socialEN.MatchString(text) {
		return DomainSocial
	}
	for _, s := range socialCJK {
		if strings.Contains(text, s) {
			return DomainSocial
		}
	}
	return DomainPractical
}

// RouteInput is the deterministic slice of a group-routing decision (excludes
// live Telegram state like reply-to and bot-exchange counters).
type RouteInput struct {
	Text        string
	MyAliases   []string // lowercased
	PeerAliases []string // lowercased
	MyDomain    string   // "practical" | "companionship" | "" (routing off)
}

// RouteDecision reports whether THIS agent should handle the message and why.
// Order: addressed-to-me wins; addressed-to-peer skips; then domain routing —
// skip only when the message CLEARLY belongs to the other domain; ambiguous or
// own-domain messages are handled. Both daemons run this identically.
func RouteDecision(in RouteInput) (handle bool, reason string) {
	if addressedTo(in.Text, in.MyAliases) {
		return true, "addressed-to-me"
	}
	if addressedTo(in.Text, in.PeerAliases) {
		return false, "addressed-to-peer"
	}
	if in.MyDomain == "" {
		return true, "no-domain-routing"
	}
	switch ClassifyGroupDomain(in.Text) {
	case DomainSocial:
		return true, "social-both-present"
	case in.MyDomain:
		return true, "my-domain"
	default:
		return false, "not-my-domain"
	}
}
