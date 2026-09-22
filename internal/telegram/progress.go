package telegram

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Progress voice (plan P3.7). The placeholder that ticks while a turn runs
// used to say "Running Bash..." in one voice for every agent. Each agent
// now owns a phrase file in its workspace and may rewrite it in its own
// voice; the handler reads it with a cheap mtime check and falls back to
// the built-in phrases the moment anything about the file is off. A bad
// phrase file can only ever make the placeholder boring, never wrong: the
// text is sent as plain text, and each phrase is validated to one short
// line with no control characters.

// ProgressPhrasesFile is the file name inside the agent's workspace.
const ProgressPhrasesFile = "progress-phrases.json"

const (
	progressMaxPhrases   = 12
	progressMaxRunes     = 60
	progressReloadPeriod = 30 * time.Second
)

// ProgressPhrases is the file's shape. Every list is optional; an empty or
// missing list falls back to the default for that slot.
type ProgressPhrases struct {
	Note     string              `json:"_note,omitempty"`
	Thinking []string            `json:"thinking"`
	Tools    map[string][]string `json:"tools"` // family → phrases; "default" catches the rest
	Long     []string            `json:"long"`
	VeryLong []string            `json:"very_long"`
}

// toolFamily maps a tool name to the family a phrase is written for. The
// raw name never reaches the screen: "mcp__ghost__ghost_put" is not
// something to show a family member, and the phrase can say what is
// happening ("remembering that") instead of what is being called.
func toolFamily(tool string) string {
	t := strings.ToLower(tool)
	switch {
	case strings.Contains(t, "search"):
		return "search"
	case strings.Contains(t, "fetch"), strings.Contains(t, "browser"), strings.Contains(t, "navigate"):
		return "browse"
	case strings.Contains(t, "ghost"), strings.Contains(t, "remember"), strings.Contains(t, "memory"):
		return "memory"
	case t == "read", t == "write", t == "edit", strings.Contains(t, "notebook"), strings.Contains(t, "glob"), strings.Contains(t, "grep"):
		return "file"
	case t == "bash", strings.Contains(t, "shell_pm"), strings.Contains(t, "tunnel"):
		return "shell"
	default:
		return "default"
	}
}

// defaultProgress is what every agent says until it writes its own file.
var defaultProgress = ProgressPhrases{
	Thinking: []string{"Thinking", "Reasoning", "Working", "Processing", "Analyzing"},
	Tools: map[string][]string{
		"search":  {"Searching the web"},
		"browse":  {"Reading a page"},
		"memory":  {"Checking my notes"},
		"file":    {"Reading files"},
		"shell":   {"Running a command"},
		"default": {"Using a tool"},
	},
	Long:     []string{"Still working (loading a lot of context)"},
	VeryLong: []string{"Still working — this one's taking a while, hang tight"},
}

// progressVoice serves phrases for one agent, reloading its file lazily.
type progressVoice struct {
	path string

	mu       sync.Mutex
	phrases  ProgressPhrases
	custom   bool
	mtime    time.Time
	nextStat time.Time
}

func newProgressVoice(path string) *progressVoice {
	return &progressVoice{path: path, phrases: defaultProgress}
}

// current returns the phrases in force, re-reading the file at most every
// progressReloadPeriod. Missing file = defaults, quietly; a present but
// unusable file = defaults, with one warning per change.
func (v *progressVoice) current() ProgressPhrases {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	if v.path == "" || now.Before(v.nextStat) {
		return v.phrases
	}
	v.nextStat = now.Add(progressReloadPeriod)
	st, err := os.Stat(v.path)
	if err != nil {
		if v.custom {
			v.phrases, v.custom, v.mtime = defaultProgress, false, time.Time{}
		}
		return v.phrases
	}
	if st.ModTime().Equal(v.mtime) {
		return v.phrases
	}
	v.mtime = st.ModTime()
	data, err := os.ReadFile(v.path)
	if err != nil {
		slog.Warn("progress phrases: read failed, using defaults", "path", v.path, "error", err)
		v.phrases, v.custom = defaultProgress, false
		return v.phrases
	}
	loaded, problems := parseProgressPhrases(data)
	for _, p := range problems {
		slog.Warn("progress phrases: "+p, "path", v.path)
	}
	v.phrases, v.custom = loaded, true
	slog.Info("progress phrases: loaded", "path", v.path, "thinking", len(loaded.Thinking), "tool_families", len(loaded.Tools))
	return v.phrases
}

// parseProgressPhrases decodes and sanitises a phrase file. Each slot keeps
// only its valid phrases and falls back to the default slot when none
// survive, so one bad line never blanks the whole voice.
func parseProgressPhrases(data []byte) (ProgressPhrases, []string) {
	var raw ProgressPhrases
	if err := json.Unmarshal(data, &raw); err != nil {
		return defaultProgress, []string{"invalid JSON, using defaults: " + err.Error()}
	}
	var problems []string
	clean := func(slot string, in []string, fallback []string) []string {
		var out []string
		for _, p := range in {
			if len(out) == progressMaxPhrases {
				problems = append(problems, slot+": more than 12 phrases, extras ignored")
				break
			}
			if q, ok := validPhrase(p); ok {
				out = append(out, q)
			} else {
				problems = append(problems, slot+": dropped an empty, multi-line or over-long phrase")
			}
		}
		if len(out) == 0 {
			return fallback
		}
		return out
	}
	out := ProgressPhrases{
		Note:     raw.Note,
		Thinking: clean("thinking", raw.Thinking, defaultProgress.Thinking),
		Long:     clean("long", raw.Long, defaultProgress.Long),
		VeryLong: clean("very_long", raw.VeryLong, defaultProgress.VeryLong),
		Tools:    map[string][]string{},
	}
	for fam, def := range defaultProgress.Tools {
		out.Tools[fam] = clean("tools."+fam, raw.Tools[fam], def)
	}
	return out, problems
}

// validPhrase trims a phrase and accepts it only as one short printable line.
func validPhrase(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || !utf8.ValidString(p) || utf8.RuneCountInString(p) > progressMaxRunes {
		return "", false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return p, true
}

// pick rotates through a list at the placeholder's pace: a phrase holds for
// five ticks (~10 s) so it reads as a sentence, not a slot machine.
func pick(list []string, tick int) string {
	if len(list) == 0 {
		return ""
	}
	return list[(tick/5)%len(list)]
}

// thinkingMessage renders the placeholder while the model is thinking. The
// ticker runs every 2 s, so tick doubles as elapsed seconds/2. Past ~20 s
// and ~60 s the phrasing switches to the long-wait slots, so a slow turn
// reads as "still working" rather than a dead "Analyzing".
func (v *progressVoice) thinkingMessage(tick int) string {
	ph := v.current()
	frame := spinnerFrames[tick%len(spinnerFrames)]
	dots := strings.Repeat(".", (tick%3)+1)
	switch {
	case tick >= 30:
		return frame + " " + pick(ph.VeryLong, tick) + dots
	case tick >= 10:
		return frame + " " + pick(ph.Long, tick) + dots
	default:
		return frame + " " + pick(ph.Thinking, tick) + dots
	}
}

// toolMessage renders what the agent is doing right now, by tool family.
func (v *progressVoice) toolMessage(tick int, tool string) string {
	ph := v.current()
	frame := spinnerFrames[tick%len(spinnerFrames)]
	dots := strings.Repeat(".", (tick%3)+1)
	list := ph.Tools[toolFamily(tool)]
	if len(list) == 0 {
		list = ph.Tools["default"]
	}
	return frame + " " + pick(list, tick) + dots
}
