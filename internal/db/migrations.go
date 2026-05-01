package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const migrationsDir = "migrations"

// baselineVersion names the initial migration shipped when forward
// migrations were introduced. Existing installs (a populated DB
// predating this system) get this version stamped without execution —
// they're already at this state, and re-executing CREATE TABLE IF NOT
// EXISTS against a long-lived install whose schema has drifted via
// post-baseline migrations would not be a faithful no-op. Fresh
// installs run the file normally to bootstrap the schema.
const baselineVersion = "20260501_001_baseline"

// runMigrations brings the on-disk schema up to head. Sequence:
//  1. Ensure schema_migrations exists.
//  2. If no migrations are recorded but application tables already
//     exist, stamp the baseline as already-applied (existing-install
//     upgrade path).
//  3. Walk migrations/*.sql lexicographically; for each version not
//     in schema_migrations, exec the file and insert the row in the
//     same transaction.
//
// File naming convention: YYYYMMDD_NNN_description.sql. The date +
// counter prefix sorts lexicographically into chronological order;
// the counter disambiguates same-day migrations. Forward-only — no
// down migrations until we have a real need.
//
// Failures roll back cleanly and surface the version that failed; the
// next launch retries the same migration. Migrations should be
// idempotent at the statement level (IF NOT EXISTS / IF EXISTS) so a
// half-applied state from a previous run can be re-driven safely.
func runMigrations(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	if err := stampBaselineIfNeeded(db); err != nil {
		return err
	}

	versions, err := listMigrationVersions()
	if err != nil {
		return err
	}

	for _, version := range versions {
		applied, err := isApplied(db, version)
		if err != nil {
			return fmt.Errorf("check schema_migrations[%s]: %w", version, err)
		}
		if applied {
			continue
		}
		if err := applyMigration(db, version); err != nil {
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		log.Printf("[db] applied migration %s", version)
	}
	return nil
}

// stampBaselineIfNeeded covers the existing-install upgrade path: a DB
// with application tables but no schema_migrations row predates this
// system, so we record the baseline as applied without executing it.
// The probe uses the entities table because (a) it's load-bearing for
// the data model and (b) it has been part of the schema since long
// before forward migrations existed, so its presence is a reliable
// "this is a real install" signal.
func stampBaselineIfNeeded(db *sql.DB) error {
	var migrationCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if migrationCount > 0 {
		return nil
	}
	var hasEntities int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'entities'`,
	).Scan(&hasEntities); err != nil {
		return fmt.Errorf("probe sqlite_master: %w", err)
	}
	if hasEntities == 0 {
		return nil
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations (version) VALUES (?)`, baselineVersion,
	); err != nil {
		return fmt.Errorf("stamp baseline: %w", err)
	}
	log.Printf("[db] stamped baseline %s on existing install (no DDL executed)", baselineVersion)
	return nil
}

func listMigrationVersions() ([]string, error) {
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		versions = append(versions, strings.TrimSuffix(name, ".sql"))
	}
	sort.Strings(versions)
	return versions, nil
}

func isApplied(db *sql.DB, version string) (bool, error) {
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func applyMigration(db *sql.DB, version string) error {
	body, err := migrationsFS.ReadFile(fmt.Sprintf("%s/%s.sql", migrationsDir, version))
	if err != nil {
		return fmt.Errorf("read %s.sql: %w", version, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (version) VALUES (?)`, version,
	); err != nil {
		return fmt.Errorf("record applied: %w", err)
	}
	return tx.Commit()
}
