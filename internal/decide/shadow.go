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
	// OnWhichProject, when set, receives every successful lane answer — the
	// live input to the router (R0) — with the question id (which_project,
	// which_project_v2). Called on the shadow's goroutine after the rows are
	// recorded.
	OnWhichProject func(t Turn, question, choice string, confidence float64, latency time.Duration)
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
		started := time.Now()
		res, err := s.decider.Ask(ctx, s.state(t), questions)
		if err != nil {
			res.Latency = time.Since(started) // a timeout is a 5 s answer, not a 0 ms one
		}
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
		if err == nil && s.OnWhichProject != nil {
			for _, qid := range []string{"which_project", "which_project_v2"} {
				if a, ok := res.Answers[qid]; ok && a.Choice != "" {
					s.OnWhichProject(t, qid, a.Choice, a.Confidence, res.Latency)
				}
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
		q["which_project"] = WhichProjectQuestion(t.Projects)
		// v2 rides in the same call (no second request): v1 stays recorded
		// for the 9/28 verdict, v2 feeds the router's jev-v2 backend.
		q["which_project_v2"] = WhichProjectQuestionV2(t.Projects)
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
	// v2 (2026-09-25): v1 ("states a decision that has been made") fired on
	// meal logs — 5 of its 6 hits in the first 1.5 days were "I ate X". A
	// new key, so the verdict never mixes the two wordings.
	q["has_decision_v2"] = Question{Type: "noul",
		Instructions: "The message settles a choice between options or commits to a plan: an option picked, a booking confirmed, a date or plan fixed. " +
			"Reporting or logging what already happened (meals eaten, activities done, a status update) is NOT a decision, and neither is a question."}
	return q, peer
}

// WhichProjectQuestion is the one lane question, shared by the live shadow
// and route replay so both ask Jev exactly the same thing. "none" is the
// general lane.
func WhichProjectQuestion(projects []Project) Question {
	crit := map[string]string{"none": "general conversation, or nothing to do with any listed project"}
	for _, p := range projects {
		desc := p.Title
		if p.Instructions != "" {
			desc += " — " + clipRunes(p.Instructions, 160)
		}
		crit[p.Slug] = desc
	}
	return Question{Type: "choice",
		Instructions: "Which ongoing family project, if any, is this message about? Choose none unless the message is clearly about one of them.",
		Criteria:     crit}
}

// WhichProjectQuestionV2 is a replay-only candidate wording. v1's "choose
// none unless clearly about one" made the first 14-day replay precise but
// blind: 100% of its project picks were right, yet it found only 28% of the
// messages a judge filed under a project, mostly meal logs sent to a meal
// and health log project. v2 says what "belongs" means. It is scored
// offline first; the live shadow keeps v1 until a replay says otherwise.
func WhichProjectQuestionV2(projects []Project) Question {
	q := WhichProjectQuestion(projects)
	q.Instructions = "Which of these ongoing projects does this message belong to? A message belongs to a project " +
		"when it feeds or continues that project's work — an entry for a log, a detail for a plan, a follow-up to it — " +
		"even if it never names the project. Choose none for conversation that serves no listed project."
	return q
}

// ShadowState is the state the shadow sends for a message (exported for
// route replay, which must send the same shape).
func ShadowState(chatKind string, threadID int64, message string) map[string]any {
	return (&Shadow{}).state(Turn{ChatKind: chatKind, ThreadID: threadID, Message: message})
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
