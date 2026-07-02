package bench

import (
	"database/sql"

	_ "modernc.org/sqlite"
)

// mustOpenSQLite opens a SQLite DB in read-write mode for the CV runner.
// Panics on failure — the caller is a test harness, not production.
func mustOpenSQLite(path string) *sql.DB {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		panic(err)
	}
	return db
}
