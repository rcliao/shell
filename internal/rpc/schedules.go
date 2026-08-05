package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// Schedule read/manage surface.
//
// Creation already existed (POST /schedule); everything here is the half that
// was missing. An agent could register a schedule and then had no way to ask
// whether it had ever run — so a wrong expression or a paused row stayed
// invisible until someone noticed the reminder never arrived. Modeled on
// Temporal's describe/list/pause/trigger operations, minus backfill, which is a
// deliberate non-goal here.

// SchedulesRequest is the request body for POST /schedules.
type SchedulesRequest struct {
	Action string `json:"action"` // list | describe | cancel | trigger
	ID     int64  `json:"id"`     // required for describe/cancel/trigger
	ChatID int64  `json:"chat_id"`
	// All includes disabled/paused rows in a list. Off by default: the common
	// question is "what is live?", and paused rows would bury it.
	All bool `json:"all"`
	// Runs caps the run-history entries returned by describe.
	Runs int `json:"runs"`
}

func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	var req SchedulesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Action == "" {
		req.Action = "list"
	}

	switch req.Action {
	case "list":
		s.schedulesList(w, req)
	case "describe":
		s.schedulesDescribe(w, req)
	case "cancel":
		s.schedulesCancel(w, req)
	case "trigger":
		s.schedulesTrigger(w, req)
	case "queue":
		s.schedulesQueue(w, req)
	default:
		writeError(w, http.StatusBadRequest, "action must be list, describe, cancel, trigger or queue")
	}
}

// schedulesQueue reports the durable execution queue behind the schedules.
//
// Firing is no longer in-memory: a fire is a queued row that survives a
// restart, gets reclaimed if its process dies, and is dropped if it goes stale.
// None of that is visible from `list` or `describe`, which read the SCHEDULE.
// Without this an agent cannot tell a beat that was replayed from one that was
// silently dropped — and a queue nobody can inspect is a queue nobody can trust.
func (s *Server) schedulesQueue(w http.ResponseWriter, req SchedulesRequest) {
	counts, err := s.store.CountTasksByState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	limit := req.Runs
	if limit <= 0 {
		limit = 10
	}
	tasks, err := s.store.ListTasks("", limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	recent := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		row := map[string]any{
			"id": t.ID, "kind": t.Kind, "state": t.State, "source": t.Source,
			"partition": t.PartitionKey, "attempts": t.Attempts,
			"enqueued_at": t.EnqueuedAt.UTC().Format(time.RFC3339),
		}
		if t.LastError != "" {
			row["last_error"] = t.LastError
		}
		recent = append(recent, row)
	}
	writeJSON(w, map[string]any{
		"counts": map[string]int{
			"queued":  counts[store.TaskQueued],
			"leased":  counts[store.TaskLeased],
			"done":    counts[store.TaskDone],
			"failed":  counts[store.TaskFailed],
			"expired": counts[store.TaskExpired],
		},
		"recent": recent,
		"note":   "leased = running now; expired = dropped as stale rather than run late; failed = attempts exhausted",
	})
}

func (s *Server) schedulesList(w http.ResponseWriter, req SchedulesRequest) {
	all, err := s.store.ListAllSchedules(!req.All)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	loc := s.location("")
	out := make([]map[string]any, 0, len(all))
	for _, sc := range all {
		if req.ChatID != 0 && sc.ChatID != req.ChatID {
			continue
		}
		out = append(out, summarize(sc, loc))
	}
	writeJSON(w, map[string]any{"schedules": out, "count": len(out)})
}

func (s *Server) schedulesDescribe(w http.ResponseWriter, req SchedulesRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required for describe")
		return
	}
	sc, err := s.store.GetScheduleByID(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sc == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no schedule with id %d", req.ID))
		return
	}

	limit := req.Runs
	if limit <= 0 {
		limit = 10
	}
	runs, err := s.store.ListJobRuns(req.ID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	loc := s.location(sc.Timezone)
	detail := summarize(*sc, loc)
	detail["message"] = sc.Message
	detail["next_runs"] = s.previewRuns(sc, loc, 3)

	history := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		entry := map[string]any{
			"at":      r.FiredAt.In(loc).Format("2006-01-02 15:04:05 MST"),
			"outcome": r.Outcome,
			"attempt": r.Attempt,
		}
		if r.DurationMS > 0 {
			entry["duration"] = (time.Duration(r.DurationMS) * time.Millisecond).String()
		}
		// Queue delay is the gap between the moment a fire was due and the
		// moment it actually started — the number that exposes a schedule
		// waiting behind other work rather than running slowly itself.
		if r.ScheduledAt != nil {
			if lag := r.FiredAt.Sub(*r.ScheduledAt); lag > 0 {
				entry["queued_for"] = lag.Round(time.Second).String()
			}
		}
		if r.ErrorMessage != "" {
			entry["error"] = r.ErrorMessage
		}
		history = append(history, entry)
	}
	detail["runs"] = history
	writeJSON(w, detail)
}

func (s *Server) schedulesCancel(w http.ResponseWriter, req SchedulesRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required for cancel")
		return
	}
	sc, err := s.store.GetScheduleByID(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sc == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no schedule with id %d", req.ID))
		return
	}
	if err := s.store.DisableSchedule(req.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Read back rather than reporting success from the write call — the same
	// discipline the write-verification work established for memory.
	after, err := s.store.GetScheduleByID(req.ID)
	if err != nil || after == nil || after.Enabled {
		writeError(w, http.StatusInternalServerError, "cancel did not take effect")
		return
	}
	writeJSON(w, map[string]any{
		"id": req.ID, "label": after.Label, "enabled": false, "status": "cancelled",
	})
}

// schedulesTrigger moves a schedule's next run to now so the next scheduler
// tick picks it up. It deliberately does NOT invoke the job inline: firing
// belongs to the scheduler, which owns the overlap policy and the ledger.
func (s *Server) schedulesTrigger(w http.ResponseWriter, req SchedulesRequest) {
	if req.ID == 0 {
		writeError(w, http.StatusBadRequest, "id is required for trigger")
		return
	}
	sc, err := s.store.GetScheduleByID(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sc == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no schedule with id %d", req.ID))
		return
	}
	if !sc.Enabled {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"schedule %d is disabled%s — re-create or re-enable it before triggering",
			req.ID, pausedSuffix(sc.PausedReason)))
		return
	}
	now := time.Now().UTC()
	if err := s.store.UpdateScheduleNextRun(req.ID, now, now); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"id":     req.ID,
		"label":  sc.Label,
		"status": "queued",
		"note":   "will fire on the next scheduler tick (within ~60s); check with action=describe",
	})
}

func summarize(sc store.Schedule, loc *time.Location) map[string]any {
	m := map[string]any{
		"id":      sc.ID,
		"label":   sc.Label,
		"type":    sc.Type,
		"mode":    sc.Mode,
		"enabled": sc.Enabled,
		"chat_id": sc.ChatID,
		"expr":    sc.Schedule,
	}
	// expected_next_at is the true occurrence; next_run_at carries firing
	// jitter, which is an implementation detail an agent should not reason about.
	next := sc.NextRunAt
	if sc.ExpectedNextAt != nil {
		next = *sc.ExpectedNextAt
	}
	if !next.IsZero() {
		m["next_run"] = next.In(loc).Format("2006-01-02 15:04 MST")
	}
	if sc.LastSuccessAt != nil {
		m["last_success"] = sc.LastSuccessAt.In(loc).Format("2006-01-02 15:04 MST")
	} else {
		m["last_success"] = "never"
	}
	if sc.PausedReason != "" {
		m["paused_reason"] = sc.PausedReason
	}
	if sc.OverlapPolicy != "" {
		m["overlap_policy"] = sc.OverlapPolicy
	}
	return m
}

func pausedSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

func (s *Server) location(tz string) *time.Location {
	if tz == "" {
		tz = s.timezone
	}
	if loc, err := time.LoadLocation(tz); err == nil && loc != nil {
		return loc
	}
	return time.UTC
}
