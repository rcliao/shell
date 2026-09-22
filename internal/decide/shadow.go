package decide

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// Shadow observes turns without touching them. For each user turn it asks
// the decider the routing questions the bridge would like answered, and
// records the answers as rows the verdict can be computed from later. It
// runs on its own goroutine with its own deadline; the turn never waits.

// shadowMaxState bounds the message text sent out: enough to classify,
// not a transcript.
const shadowMaxState = 2000

// PeerTurnMarker is the prefix the bridge puts on a message relayed from
// the other agent (see a2a.go). Those are the turns where "should I reply"
// is a real decision today made by a chat-model turn ending in [noop].
const PeerTurnMarker = "(your fellow agent) said this in the group"

// Project is the little the shadow needs to know about a project.
type Project struct {
	Slug, Title, Instructions string
	ThreadID                  int64
}

// Turn is the shadow's view of one user turn.
type Turn struct {
	ChatID, ThreadID, MsgID int64
	ChatKind                string // "group" | "dm"
	Message                 string
	Projects                []Project // ACTIVE projects bound to this chat
	// Facts recorded beside the answers, for ground truth:
	BoundProject string // slug of the project whose own thread this is ("" if none)
}

// Row is one recorded answer — one row per question per turn.
type Row struct {
	ChatID, ThreadID, MsgID int64
	Question                string
	Candidates              string // JSON list of option keys ("" for noul)
	Choice                  string
	Probabilities           string // JSON map
	Confidence              float64
	Noul                    float64
	BoundProject            string
	PeerTurn                bool
	LatencyMs               int64
	InputTokens             int
	OutputTokens            int
	Error                   string
	Model                   string
}

// Recorder persists rows. The store implements it; tests use a slice.
type Recorder interface {
	LogRouterDecision(Row) error
}

// Shadow ties a Decider to a Recorder.
type Shadow struct {
	decider  Decider
	recorder Recorder
	timeout  time.Duration
}

func NewShadow(d Decider, r Recorder) *Shadow {
	return &Shadow{decider: d, recorder: r, timeout: 5 * time.Second}
}

// Observe asks and records in the background. Returns at once. Safe to
// call with a nil receiver or a disabled decider: it then does nothing.
func (s *Shadow) Observe(t Turn) {
	if s == nil || s.decider == nil || !s.decider.Enabled() || s.recorder == nil {
		return
	}
	questions, peer := s.questions(t)
	if len(questions) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		res, err := s.decider.Ask(ctx, s.state(t), questions)
		for id, q := range questions {
			row := Row{ChatID: t.ChatID, ThreadID: t.ThreadID, MsgID: t.MsgID, Question: id,
				BoundProject: t.BoundProject, PeerTurn: peer, Model: res.Model,
				LatencyMs: res.Latency.Milliseconds(), InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens}
			if q.Type == "choice" {
				keys := make([]string, 0, len(q.Criteria))
				for k := range q.Criteria {
					keys = append(keys, k)
				}
				row.Candidates = mustJSON(keys)
			}
			if err != nil {
				row.Error = err.Error()
			} else if a, ok := res.Answers[id]; ok {
				row.Choice, row.Confidence, row.Noul = a.Choice, a.Confidence, a.Noul
				if a.Probabilities != nil {
					row.Probabilities = mustJSON(a.Probabilities)
				}
			} else {
				row.Error = "no answer for question"
			}
			if rerr := s.recorder.LogRouterDecision(row); rerr != nil {
				slog.Warn("router shadow: record failed", "question", id, "error", rerr)
			}
		}
		if err != nil {
			slog.Warn("router shadow: ask failed", "chat_id", t.ChatID, "error", err)
		} else {
			slog.Info("router shadow: answered", "chat_id", t.ChatID, "questions", len(questions),
				"latency_ms", res.Latency.Milliseconds(), "input_tokens", res.Usage.InputTokens)
		}
	}()
}

// state is what the model sees: the message and where it was said. Never
// the transcript, never memory.
func (s *Shadow) state(t Turn) map[string]any {
	msg := t.Message
	if i := strings.Index(msg, "]\n"); strings.Contains(msg[:min(len(msg), 200)], PeerTurnMarker) && i > 0 {
		msg = msg[i+2:] // drop the relay frame; the model should judge the words
	}
	return map[string]any{
		"chat":    t.ChatKind,
		"thread":  ifThen(t.ThreadID != 0, "a topic thread", "the general thread"),
		"message": clipRunes(msg, shadowMaxState),
	}
}

// questions builds the batch for this turn. which_project only when the
// chat has projects; should_reply only on peer turns; the two nouls always.
func (s *Shadow) questions(t Turn) (map[string]Question, bool) {
	q := map[string]Question{}
	if len(t.Projects) > 0 {
		crit := map[string]string{"none": "general conversation, or nothing to do with any listed project"}
		for _, p := range t.Projects {
			desc := p.Title
			if p.Instructions != "" {
				desc += " — " + clipRunes(p.Instructions, 160)
			}
			crit[p.Slug] = desc
		}
		q["which_project"] = Question{Type: "choice",
			Instructions: "Which ongoing family project, if any, is this message about? Choose none unless the message is clearly about one of them.",
			Criteria:     crit}
	}
	peer := strings.Contains(t.Message[:min(len(t.Message), 200)], PeerTurnMarker)
	if peer {
		q["should_reply"] = Question{Type: "choice",
			Instructions: "Another assistant in this family group chat already replied to this message. Should this assistant add a reply as well?",
			Criteria: map[string]string{
				"reply": "it has something genuinely useful and different to add, or a part of the task to take",
				"noop":  "nothing to add; a second reply would be noise",
			}}
	}
	q["has_open_question"] = Question{Type: "noul",
		Instructions: "The message asks a question or raises a choice that still needs a human decision."}
	q["has_decision"] = Question{Type: "noul",
		Instructions: "The message states a decision that has been made (a choice settled, a booking done, a plan fixed)."}
	return q, peer
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func ifThen(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
