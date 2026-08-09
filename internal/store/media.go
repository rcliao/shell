package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
)

// Inbound media ledger (V2-H19 vision memory).
//
// Every photo a user sends is archived to a persistent path and recorded
// here, so 「剛剛那張照片」 keeps working across turns, rotations, and days —
// the old temp-file flow expired mid-conversation (7/11: 「暫存檔案也已經
// 過期了，讀不到了」). Description is filled in after the answering turn via
// the [media-note] marker, making photos text-searchable.

// MediaRow is one archived inbound photo.
type MediaRow struct {
	ID          int64
	ChatID      int64
	ThreadID    int64
	MsgID       int
	Path        string
	Caption     string
	Description string
	CreatedAt   time.Time
	// SHA256 is the digest of the bytes at archive time, lowercase hex. Empty
	// for rows written before content addressing existed — those are
	// unverifiable, which is a different state from corrupt.
	SHA256    string
	SizeBytes int64
}

// RecordMedia ledgers an archived inbound photo and returns its row id.
//
// The digest is taken at archive time rather than on read, because the point
// is to catch a file that was already wrong when it landed — a truncated
// download or a half-completed move. Computing it later would only ever agree
// with whatever bytes survived.
func (s *Store) RecordMedia(chatID, threadID int64, msgID int, path, caption, sha256Hex string, sizeBytes int64) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO media (chat_id, thread_id, msg_id, path, caption, sha256, size_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		chatID, threadID, msgID, path, caption, sha256Hex, sizeBytes)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MediaVerdict is the outcome of checking one archived file against its digest.
type MediaVerdict struct {
	Row    MediaRow
	Status string // ok | corrupt | missing | unverifiable
	Detail string
}

// Media verdicts. `unverifiable` is deliberately distinct from `corrupt`: a row
// archived before digests existed cannot be judged, and reporting it as damaged
// would cry wolf across the whole back catalogue.
const (
	MediaOK           = "ok"
	MediaCorrupt      = "corrupt"
	MediaMissing      = "missing"
	MediaUnverifiable = "unverifiable"
)

// AllMedia returns every ledgered photo, oldest first, for verification sweeps.
func (s *Store) AllMedia() ([]MediaRow, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, thread_id, msg_id, path, caption, description,
		       created_at, sha256, size_bytes
		FROM media ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRow
	for rows.Next() {
		var r MediaRow
		if err := rows.Scan(&r.ID, &r.ChatID, &r.ThreadID, &r.MsgID, &r.Path,
			&r.Caption, &r.Description, &r.CreatedAt, &r.SHA256, &r.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetMediaDescription stores the agent's own one-line understanding of the
// photo (from the [media-note] marker).
func (s *Store) SetMediaDescription(id int64, desc string) error {
	_, err := s.db.Exec(`UPDATE media SET description = ? WHERE id = ?`, desc, id)
	return err
}

// RecentMedia returns the newest archived photos for a chat, newest first.
func (s *Store) RecentMedia(chatID, threadID int64, limit int) ([]MediaRow, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, thread_id, msg_id, path, caption, description, created_at
		FROM media WHERE chat_id = ? AND thread_id = ?
		ORDER BY id DESC LIMIT ?`,
		chatID, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRow
	for rows.Next() {
		var r MediaRow
		if err := rows.Scan(&r.ID, &r.ChatID, &r.ThreadID, &r.MsgID, &r.Path, &r.Caption, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fileDigest returns the lowercase-hex sha256 and byte length of a file.
//
// Streamed rather than read whole: the archive holds hundreds of photos and a
// verification sweep walks all of them, so buffering each one entirely would
// make the sweep's memory use proportional to the largest file for no gain.
func FileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// VerifyMedia re-digests every ledgered photo and reports what it finds.
//
// Deliberately four outcomes, not two. "missing" and "corrupt" have different
// causes and different fixes, and "unverifiable" covers rows archived before
// digests existed — calling those corrupt would condemn the whole back
// catalogue on the day this shipped.
func (s *Store) VerifyMedia() ([]MediaVerdict, error) {
	rows, err := s.AllMedia()
	if err != nil {
		return nil, err
	}
	out := make([]MediaVerdict, 0, len(rows))
	for _, r := range rows {
		v := MediaVerdict{Row: r}
		switch {
		case r.SHA256 == "":
			v.Status = MediaUnverifiable
			v.Detail = "archived before content addressing"
		default:
			sum, size, derr := FileDigest(r.Path)
			switch {
			case os.IsNotExist(derr):
				v.Status, v.Detail = MediaMissing, "file is gone"
			case derr != nil:
				v.Status, v.Detail = MediaMissing, derr.Error()
			case sum != r.SHA256:
				v.Status = MediaCorrupt
				v.Detail = fmt.Sprintf("digest %s != %s (%d bytes on disk, %d recorded)",
					sum[:12], r.SHA256[:12], size, r.SizeBytes)
			default:
				v.Status = MediaOK
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// BackfillMediaDigest records a digest for a row that predates content
// addressing.
//
// Guarded to empty digests only. A backfill that could overwrite an existing
// digest would quietly repair the very corruption the ledger exists to expose:
// re-hashing a damaged file and storing the result makes it verify forever
// after. Existing digests are immutable by design.
func (s *Store) BackfillMediaDigest(id int64, sha256Hex string, sizeBytes int64) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE media SET sha256 = ?, size_bytes = ?
		WHERE id = ? AND sha256 = ''`, sha256Hex, sizeBytes, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
