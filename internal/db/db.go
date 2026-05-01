package db

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps a sql.DB connection for passing to subsystems.
type DB struct {
	Conn *sql.DB
}

// Open returns a connection to the SQLite database at ~/.triagefactory/triagefactory.db.
// Creates the directory if it doesn't exist.
func Open() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(home, ".triagefactory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dir, "triagefactory.db")
	// busy_timeout(5000) is the safety net: modernc.org/sqlite returns
	// SQLITE_BUSY immediately on lock contention unless this is set,
	// unlike mattn/go-sqlite3 which had implicit driver-level retries.
	// 5s gives any rare contention plenty of room to resolve before
	// surfacing an error.
	//
	// _time_format=sqlite forces modernc to serialize time.Time bind
	// parameters as "2006-01-02 15:04:05.999999999-07:00" instead of
	// the default Go time.String() form ("2006-01-02 15:04:05 -0700
	// MST [m=+...]"), which is unparseable by SQLite date functions
	// and by anyone reading the column as TEXT (e.g. via COALESCE in
	// factory queries). Direct time.Time scans against legacy rows
	// already in the old format still succeed — modernc's reader is
	// permissive — so no data migration is needed.
	db, err := sql.Open("sqlite", dbPath+
		"?_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(on)"+
		"&_pragma=busy_timeout(5000)"+
		"&_time_format=sqlite")
	if err != nil {
		return nil, err
	}

	// A single connection serializes this process's DB work at the
	// Go-pool layer, eliminating in-process races for SQLite's file
	// lock by queueing contention in Go instead. SQLITE_BUSY can still
	// happen from external contention, such as another process holding
	// a write transaction or a long-running read transaction, so the
	// busy_timeout above remains an important backstop. WAL still allows
	// other processes (e.g. `triagefactory exec` invocations) to read
	// concurrently against the same file. SetConnMaxLifetime(0) keeps
	// the one connection alive for the process lifetime so we don't
	// pay reconnect cost on idle gaps.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

// Migrate brings the schema to head by applying any pending forward
// migrations from internal/db/migrations/*.sql. Idempotent — running
// against an up-to-date DB is a no-op. Existing installs (DBs that
// predate the migration runner) are stamped at the baseline on first
// run; their schema is not re-executed. See migrations.go for the
// runner and the existing-install upgrade path.
func Migrate(db *sql.DB) error {
	return runMigrations(db)
}
