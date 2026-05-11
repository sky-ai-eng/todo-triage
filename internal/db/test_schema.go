package db

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// BootstrapSchemaForTest applies the full schema and seed data to db
// from a cached SQL bundle. Equivalent to Migrate + SeedEventTypes,
// but the bundle is built once per process — each test pays one Exec
// instead of running goose's full Up cycle plus the event-types seed.
//
// The bundle is captured by running Migrate + SeedEventTypes against
// a fresh in-memory template, then dumping the resulting schema via
// sqlite_master plus rows from goose_db_version (so a follow-up
// Migrate call sees head) and events_catalog (FK target most tests
// rely on). The migration runner itself is still covered by
// migrations_test.go, which uses Migrate directly.
//
// Tests-only. Production code uses Migrate.
func BootstrapSchemaForTest(db *sql.DB) error {
	bundle, err := cachedSchemaBundle()
	if err != nil {
		return err
	}
	_, err = db.Exec(bundle)
	return err
}

var (
	schemaBundleOnce sync.Once
	schemaBundleSQL  string
	schemaBundleErr  error
)

func cachedSchemaBundle() (string, error) {
	schemaBundleOnce.Do(func() {
		schemaBundleSQL, schemaBundleErr = buildSchemaBundle()
	})
	return schemaBundleSQL, schemaBundleErr
}

func buildSchemaBundle() (string, error) {
	template, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		return "", fmt.Errorf("open template: %w", err)
	}
	defer template.Close()
	template.SetMaxOpenConns(1)
	template.SetMaxIdleConns(1)

	if err := Migrate(template, "sqlite3"); err != nil {
		return "", fmt.Errorf("migrate template: %w", err)
	}
	if err := SeedEventTypes(template); err != nil {
		return "", fmt.Errorf("seed template: %w", err)
	}

	var b strings.Builder

	// DDL in sqlite_master rowid order so any dependency ordering
	// observed during creation is preserved on replay.
	rows, err := template.Query(`
		SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL
		  AND name NOT LIKE 'sqlite_%'
		ORDER BY rowid
	`)
	if err != nil {
		return "", fmt.Errorf("dump sqlite_master: %w", err)
	}
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			rows.Close()
			return "", err
		}
		b.WriteString(stmt)
		b.WriteString(";\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	// Seed rows: goose_db_version so a follow-up Migrate sees head
	// (post-SKY-245 the runner is goose-managed; the legacy
	// schema_migrations table is no longer created on fresh installs);
	// events_catalog because it's the FK target for task_rules.event_type
	// and prompt_triggers.event_type and many tests insert against it.
	if err := dumpTableInserts(template, "goose_db_version", &b); err != nil {
		return "", err
	}
	if err := dumpTableInserts(template, "events_catalog", &b); err != nil {
		return "", err
	}

	return b.String(), nil
}

func dumpTableInserts(db *sql.DB, table string, w *strings.Builder) error {
	cols, err := tableColumns(db, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT %s FROM %s`,
		strings.Join(cols, ", "), table))
	if err != nil {
		return err
	}
	defer rows.Close()

	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	colList := strings.Join(cols, ", ")
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		w.WriteString("INSERT INTO ")
		w.WriteString(table)
		w.WriteString(" (")
		w.WriteString(colList)
		w.WriteString(") VALUES (")
		for i, v := range values {
			if i > 0 {
				w.WriteString(", ")
			}
			w.WriteString(sqlLiteral(v))
		}
		w.WriteString(");\n")
	}
	return rows.Err()
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notnull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func sqlLiteral(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case []byte:
		return "x'" + hex.EncodeToString(v) + "'"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case time.Time:
		return "'" + v.UTC().Format("2006-01-02 15:04:05.999999999") + "'"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", "''") + "'"
	}
}
