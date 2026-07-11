package bridge

import (
	"regexp"
	"strings"
)

// fableModel is the model a "fable"-keyworded turn runs on. Kept here (not in
// per-agent config) so the experiment behaves identically for every agent.
const fableModel = "claude-fable-5"

// fableKeywordRe matches the standalone token "fable" (case-insensitive),
// ultracode-style: including it in an otherwise-normal message routes THAT turn
// to Fable. \b boundaries mean it won't fire inside other words, and it still
// matches when butted against CJK ("黃了fable") since CJK is a non-word char.
var fableKeywordRe = regexp.MustCompile(`(?i)\bfable\b`)

// detectFableKeyword reports whether the message opts this turn into Fable and
// returns the message with the keyword removed (and whitespace tidied). It only
// triggers when non-keyword content remains — a bare "fable" is left alone so
// it flows to the default model as ordinary text rather than an empty prompt.
func detectFableKeyword(userMsg string) (fable bool, stripped string) {
	if !fableKeywordRe.MatchString(userMsg) {
		return false, userMsg
	}
	stripped = strings.TrimSpace(collapseSpaces(fableKeywordRe.ReplaceAllString(userMsg, " ")))
	if stripped == "" {
		return false, userMsg
	}
	return true, stripped
}

var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

func collapseSpaces(s string) string {
	return multiSpaceRe.ReplaceAllString(s, " ")
}
