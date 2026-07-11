package telegram

import (
	"regexp"
	"strings"
)

// Role-based group routing.
//
// Every group message reaches BOTH agents. To make exactly one answer a general
// (unaddressed) message — deterministically, without the two daemons
// coordinating — both run this identical classifier and each answers only its
// own domain. Assignment lives in config (agent.group_domain): pikamini owns
// "practical" (the default), umbreonmini owns "companionship". A named/@mention
// message overrides this (the addressed agent answers regardless).

// DomainPractical is the default domain — household/food/plants/products/how-to.
const DomainPractical = "practical"

// DomainCompanionship covers feelings, mood, comfort, reflection, "how are you".
const DomainCompanionship = "companionship"

// companionshipEN are English cues that a message is emotional/companionship-
// seeking rather than a practical task. Word-boundary matched.
var companionshipEN = regexp.MustCompile(`(?i)\b(feel|feeling|feelings|tired|exhausted|sad|lonely|miss you|love you|stressed|anxious|worried|overwhelmed|comfort|cheer me|vent|upset|cry|crying|hug|proud of|how are you|are you ok|are you okay|talk to you|chat with you)\b`)

// companionshipCJK are Chinese cues matched as substrings (deliberately a tight,
// clearly-emotional set — practical is the safe default, so only route to
// companionship on a strong signal).
var companionshipCJK = []string{
	"累", "好累", "心累", "難過", "傷心", "心情", "想你", "想妳", "陪我", "陪陪",
	"孤單", "寂寞", "壓力", "焦慮", "擔心", "抱抱", "想哭", "委屈", "難受",
	"心疼", "愛你", "愛妳", "你好嗎", "還好嗎", "聊聊", "傾訴", "撐不住",
	"煩死", "好煩", "情緒", "陪伴",
}

// ClassifyGroupDomain returns the routing domain for a general group message.
// Deterministic: same input → same output on both daemons.
func ClassifyGroupDomain(text string) string {
	if companionshipEN.MatchString(text) {
		return DomainCompanionship
	}
	for _, s := range companionshipCJK {
		if strings.Contains(text, s) {
			return DomainCompanionship
		}
	}
	return DomainPractical
}
