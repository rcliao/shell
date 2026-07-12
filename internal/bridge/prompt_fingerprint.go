package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
)

// promptFingerprintKey is the kv key holding the last-seen static-prompt hash.
const promptFingerprintKey = "prompt_fingerprint"

// staticSystemPrompt returns the process-static portion of the system prompt —
// everything a fresh (non-onboarding) send appends EXCEPT per-chat pinned memory,
// which prefix_hash already tracks: identity + time guidance + skills catalog +
// session-lifecycle note + group rules. Its hash is the "prompt fingerprint".
func (b *Bridge) staticSystemPrompt() string {
	s := b.agentIdentity + b.timestampSystemPrompt() + b.skillsSystemPrompt() + b.sessionLifecyclePrompt()
	if b.transcript != nil {
		s += b.groupAgentPrompt()
	}
	return s
}

// promptFingerprint hashes the static system prompt so a deploy (new binary with
// changed prompt text) or a skill reload that alters it can be detected.
func (b *Bridge) promptFingerprint() string {
	sum := sha256.Sum256([]byte(b.staticSystemPrompt()))
	return hex.EncodeToString(sum[:])
}

// ReconcilePromptFingerprint compares the current static system-prompt fingerprint
// against the last-seen one (persisted in kv). If it changed — a new binary with
// different prompt text, or a live skill reload — every active session is flagged
// to rotate onto the new prompt on its next turn, instead of serving the STALE
// cached prompt until an unrelated (cost/day) rotation or a manual flag.
//
// This closes the long-standing gap where prefix_hash only tracked pinned memory,
// so skills/identity/prompt-section changes never triggered rotation. Call at
// startup (after identity+skills+transcript are wired) and after ReloadSkills.
// Idempotent: a no-op when nothing changed; on first-ever run it only records the
// baseline (no flag), so future changes propagate automatically.
func (b *Bridge) ReconcilePromptFingerprint() {
	if b.store == nil {
		return
	}
	current := b.promptFingerprint()
	prev, ok, err := b.store.GetKV(promptFingerprintKey)
	if err != nil {
		slog.Warn("prompt-fingerprint: read failed", "error", err)
		return
	}
	if ok && prev == current {
		return // unchanged — nothing to do
	}
	if ok {
		n, ferr := b.store.FlagActiveSessionsForRotation("prompt_changed")
		if ferr != nil {
			slog.Warn("prompt-fingerprint: flag failed", "error", ferr)
			return
		}
		slog.Info("prompt-fingerprint changed — flagged active sessions to rotate onto new system prompt", "flagged", n)
	}
	if err := b.store.SetKV(promptFingerprintKey, current); err != nil {
		slog.Warn("prompt-fingerprint: persist failed", "error", err)
	}
}
