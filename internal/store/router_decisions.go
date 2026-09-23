package store

import "github.com/rcliao/shell/internal/decide"

// LogRouterDecision persists one shadow-router observation (plan P3.7).
// Best-effort like the other ledgers; the caller only warns.
func (s *Store) LogRouterDecision(r decide.Row) error {
	_, err := s.db.Exec(`
		INSERT INTO router_decisions
		  (chat_id, thread_id, msg_id, question, candidates, choice, probabilities,
		   confidence, noul, bound_project, peer_turn, latency_ms, input_tokens,
		   output_tokens, error, model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ChatID, r.ThreadID, r.MsgID, r.Question, r.Candidates, r.Choice, r.Probabilities,
		r.Confidence, r.Noul, r.BoundProject, boolInt(r.PeerTurn), r.LatencyMs, r.InputTokens,
		r.OutputTokens, r.Error, r.Model)
	return err
}
