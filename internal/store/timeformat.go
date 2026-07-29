package store

import "fmt"

// Timestamp format normalization.
//
// Before _time_format=sqlite was set on the DSN, the driver wrote time.Time
// through t.String(), producing values like "2026-07-29 17:11:17.34 +0000 UTC".
// SQLite's date functions cannot parse that tail, so datetime(), julianday()
// and any `WHERE ts < datetime('now', '-30 days')` sweep silently returned
// nothing for those rows. Go-side reads always worked, which is why the
// breakage stayed invisible until someone queried the ledger by hand.
//
// The rewrite is a pure suffix swap: " +0000 UTC" -> "+00:00", which turns the
// value into parseTimeFormats[0] — the same format the driver now writes, and
// one SQLite understands. Only exact-UTC rows are touched; every timestamp
// these tables have ever held is written UTC, and a row in some other zone is
// left alone rather than guessed at (Go still reads it fine).
//
// Scoped to the tables that get date-queried: schedules drives firing and the
// dead-man's-switch, job_runs is the reliability ledger and the future
// retention target. Other tables keep mixed formats harmlessly — reads accept
// both, and lexical ordering is unaffected because the two formats agree on
// every character up to the fractional seconds.

// timeColumns lists the columns normalized by migrateTimeFormat.
var timeColumns = map[string][]string{
	"schedules": {"next_run_at", "last_run_at", "expected_next_at", "last_success_at"},
	"job_runs":  {"fired_at", "scheduled_at"},
}

const legacyUTCSuffix = " +0000 UTC"

// migrateTimeFormat rewrites legacy t.String()-formatted UTC timestamps into a
// format SQLite's date functions can parse. Idempotent: the WHERE clause
// matches only rows still carrying the legacy suffix.
func (s *Store) migrateTimeFormat() error {
	for table, cols := range timeColumns {
		for _, col := range cols {
			stmt := fmt.Sprintf(
				`UPDATE %s SET %s = replace(%s, ?, '+00:00') WHERE %s LIKE ?`,
				table, col, col, col)
			if _, err := s.db.Exec(stmt, legacyUTCSuffix, "%"+legacyUTCSuffix); err != nil {
				return fmt.Errorf("%s.%s: %w", table, col, err)
			}
		}
	}
	return nil
}
