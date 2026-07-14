package bridge

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/config"
)

// Inbound media archiving (V2-H19 vision memory).
//
// Photos used to live in os.CreateTemp and expired mid-conversation. Now the
// handler archives each inbound photo here before the turn runs: the file
// moves to a persistent dir, a ledger row is created, and later turns can
// Read the archived path again (injected via Channel B). The answering turn
// is asked to emit a [media-note: ...] one-liner which becomes the photo's
// searchable description.

// mediaNoteRe matches the trailing [media-note: ...] marker the agent emits
// on photo turns. Stripped before delivery.
var mediaNoteRe = regexp.MustCompile(`\[media-note:?\s*([^\]]+)\]`)

// ArchiveInboundMedia moves a downloaded photo from its temp path into the
// persistent media archive and ledgers it. On success img.Path points at the
// archived file and img.MediaID at the ledger row. Failures leave the temp
// path in place — the turn still works, only persistence is lost.
func (b *Bridge) ArchiveInboundMedia(chatID, threadID int64, msgID int, caption string, img *ImageInfo) {
	if b.store == nil || img == nil || img.Path == "" {
		return
	}
	now := time.Now()
	dir := filepath.Join(config.DefaultConfigDir(), "media", now.Format("2006-01"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("media archive: mkdir failed, keeping temp path", "error", err)
		return
	}
	// ~/.shell/media is shared across agent daemons (same convention as
	// artifacts/): prefix with the agent name so files stay attributable and
	// the two agents' copies of the same group photo don't collide.
	dest := filepath.Join(dir, fmt.Sprintf("%s-%s-msg%d%s",
		b.agentBotUsername, now.Format("20060102-150405"), msgID, filepath.Ext(img.Path)))
	if err := os.Rename(img.Path, dest); err != nil {
		data, rerr := os.ReadFile(img.Path)
		if rerr != nil || os.WriteFile(dest, data, 0o644) != nil {
			slog.Warn("media archive: move failed, keeping temp path", "path", img.Path, "error", err)
			return
		}
		os.Remove(img.Path)
	}
	img.Path = dest
	id, err := b.store.RecordMedia(chatID, threadID, msgID, dest, caption)
	if err != nil {
		slog.Warn("media archive: ledger write failed", "path", dest, "error", err)
		return
	}
	img.MediaID = id
	slog.Info("inbound media archived", "chat_id", chatID, "media_id", id, "dest", dest)
}

// mediaNoteInstruction asks the answering turn to describe the attached
// photo(s) so the archive becomes searchable. The marker is stripped before
// delivery.
func mediaNoteInstruction(n int) string {
	return fmt.Sprintf("[This message includes %d archived photo(s). After your answer, append one line exactly like: [media-note: <dense one-line description: main subject, setting/scene, any text visible in the image, people by name if you recognize them, and what is happening or why it was shared>] — it is stripped before delivery and saved as a memory so you and future sessions can find and recognize this photo later.]", n)
}

// humanAge renders a duration as a compact age ("35m ago", "6h ago", "3d ago").
func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// extractMediaNote pulls the [media-note] marker out of a response, returning
// the cleaned response and the note text ("" if absent).
func extractMediaNote(response string) (cleaned, note string) {
	m := mediaNoteRe.FindStringSubmatch(response)
	if m == nil {
		return response, ""
	}
	note = strings.TrimSpace(m[1])
	cleaned = strings.TrimSpace(mediaNoteRe.ReplaceAllString(response, ""))
	return cleaned, note
}
