package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// promptStore is the Postgres impl of db.PromptStore.
//
// # Pool split
//
// Most methods run on the app pool (tf_app, RLS-active). SeedOrUpdate
// is the exception — system_prompt_versions has INSERT/UPDATE/DELETE
// REVOKE'd from tf_app per D3, so the sidecar write can ONLY happen
// on the admin pool. The impl holds both pools at construction and
// picks per-method.
//
// # Composite PK + RLS
//
// prompts has PRIMARY KEY (org_id, id) and UNIQUE (id, org_id) (the
// second exists so child tables like projects can use composite FKs
// targeting prompts). Every method includes org_id in WHERE clauses
// as defense in depth alongside RLS — if RLS were ever misconfigured
// or bypassed the org filter still applies.
//
// # Type mappings vs SQLite
//
//   - hidden / user_modified are BOOLEAN here vs INTEGER 0/1 in SQLite.
//     Reads scan into bool; the wire shape (JSON) is identical.
//   - created_at / updated_at are TIMESTAMPTZ; time.Time scans cleanly.
//   - DATE() doesn't exist as a function — use `started_at::date`.
type promptStore struct {
	app   queryer
	admin queryer
	inTx  bool // when constructed inside WithTx, both fields point at the same *sql.Tx
}

func newPromptStore(app, admin queryer) db.PromptStore {
	return &promptStore{app: app, admin: admin}
}

func newTxPromptStore(tx queryer) db.PromptStore {
	// Inside a tx, SeedOrUpdate cannot escape to the admin pool —
	// it would break tx semantics and bypass the caller's WithTx
	// scope. SeedOrUpdate inside a tx-bound store returns an error
	// (see method body); the only production caller is the startup
	// seeder, which runs outside any tx.
	return &promptStore{app: tx, admin: tx, inTx: true}
}

var _ db.PromptStore = (*promptStore)(nil)

// --- SeedOrUpdate --------------------------------------------------

func (s *promptStore) SeedOrUpdate(ctx context.Context, orgID string, p domain.Prompt) error {
	if s.inTx {
		// system_prompt_versions writes need the admin pool;
		// inside WithTx we have an app-pool tx. Escaping to the
		// admin pool would silently bypass the caller's tx scope.
		// The startup seeder is the only legit caller and runs
		// outside WithTx.
		return errors.New("postgres prompts: SeedOrUpdate must not be called inside WithTx; call stores.Prompts.SeedOrUpdate directly")
	}
	if p.Source == "" {
		p.Source = "system"
	}
	if p.Source != "system" {
		return fmt.Errorf("postgres prompts: SeedOrUpdate only accepts Source=\"system\" (got %q); use Create or UpdateImported for non-system rows", p.Source)
	}
	hash := shippedContentHash(p)
	now := time.Now().UTC()

	// Open a tx on the admin pool so the prompt + version row writes
	// commit atomically. Admin bypasses RLS so no JWT claim plumbing
	// is needed — supabase_admin sees every row.
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres prompts: SeedOrUpdate requires a *sql.DB admin handle, got %T", s.admin)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		exists       bool
		userModified bool
	)
	switch err := tx.QueryRowContext(ctx,
		`SELECT user_modified FROM prompts WHERE org_id = $1 AND id = $2`, orgID, p.ID,
	).Scan(&userModified); {
	case errors.Is(err, sql.ErrNoRows):
		exists = false
	case err != nil:
		return fmt.Errorf("read prompt: %w", err)
	default:
		exists = true
	}

	if !exists {
		// creator_user_id is NOT NULL. SeedOrUpdate runs admin-pool
		// (no JWT claim → tf.current_user_id() is NULL), so the
		// COALESCE falls back to the org's founder. Same pattern as
		// Create — "org owner authored this" is the natural reading
		// for shipped system prompts seeded at deploy time.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompts (id, org_id, creator_user_id, name, body, source, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2,
				COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
				$3, $4, $5, 0, FALSE, $6, $6)
		`, p.ID, orgID, p.Name, p.Body, p.Source, now); err != nil {
			return fmt.Errorf("insert prompt: %w", err)
		}
		if err := upsertSystemPromptVersionPG(ctx, tx, orgID, p.ID, hash, now); err != nil {
			return err
		}
		return tx.Commit()
	}

	if userModified {
		return tx.Commit()
	}

	var priorHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT content_hash FROM system_prompt_versions WHERE org_id = $1 AND prompt_id = $2`, orgID, p.ID,
	).Scan(&priorHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read prior prompt version: %w", err)
	}
	if priorHash.Valid && priorHash.String == hash {
		return tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE prompts
		SET name = $1, body = $2, source = $3, updated_at = $4
		WHERE org_id = $5 AND id = $6
	`, p.Name, p.Body, p.Source, now, orgID, p.ID); err != nil {
		return fmt.Errorf("update prompt: %w", err)
	}
	if err := upsertSystemPromptVersionPG(ctx, tx, orgID, p.ID, hash, now); err != nil {
		return err
	}
	return tx.Commit()
}

// shippedContentHash digests the shipped (name, body, source) triple
// with null-byte separators. Identical to the SQLite helper — kept
// in this package too because Go's package-private visibility forbids
// the SQLite version reaching here, and duplicating 4 lines beats
// pulling a "shared internals" package for one helper.
func shippedContentHash(p domain.Prompt) string {
	h := sha256.Sum256([]byte(p.Name + "\x00" + p.Body + "\x00" + p.Source))
	return hex.EncodeToString(h[:])
}

func upsertSystemPromptVersionPG(ctx context.Context, q queryer, orgID, promptID, hash string, now time.Time) error {
	// applied_at only bumps when content_hash actually changed —
	// the WHERE on the conflict branch makes identical-hash
	// reseeds a no-op even for legacy rows that fall through to
	// this upsert (the seed body's own short-circuit would also
	// have prevented this for non-legacy rows).
	if _, err := q.ExecContext(ctx, `
		INSERT INTO system_prompt_versions (org_id, prompt_id, content_hash, applied_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (org_id, prompt_id) DO UPDATE SET
			content_hash = EXCLUDED.content_hash,
			applied_at = EXCLUDED.applied_at
		WHERE system_prompt_versions.content_hash <> EXCLUDED.content_hash
	`, orgID, promptID, hash, now); err != nil {
		return fmt.Errorf("upsert system prompt version: %w", err)
	}
	return nil
}

// --- CRUD ----------------------------------------------------------

func (s *promptStore) List(ctx context.Context, orgID string) ([]domain.Prompt, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT id, name, body, source, allowed_tools, usage_count, created_at, updated_at
		FROM prompts WHERE org_id = $1 AND hidden = FALSE ORDER BY updated_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var prompts []domain.Prompt
	for rows.Next() {
		var p domain.Prompt
		if err := rows.Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.AllowedTools, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

func (s *promptStore) Get(ctx context.Context, orgID string, id string) (*domain.Prompt, error) {
	var p domain.Prompt
	err := s.app.QueryRowContext(ctx, `
		SELECT id, name, body, source, allowed_tools, usage_count, created_at, updated_at
		FROM prompts WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan(&p.ID, &p.Name, &p.Body, &p.Source, &p.AllowedTools, &p.UsageCount, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *promptStore) Create(ctx context.Context, orgID string, p domain.Prompt) error {
	// creator_user_id is NOT NULL. Two execution contexts:
	//   - Request path: WithTx has set request.jwt.claims, so
	//     tf.current_user_id() returns the caller's UUID. That's
	//     the right audit identity.
	//   - System / deploy-time path (or tests against admin pool
	//     with no claim): tf.current_user_id() returns NULL.
	//     Fall back to the org's founder (orgs.owner_user_id) so
	//     the constraint is always satisfied with a meaningful FK
	//     target. "Org founder created this" is the natural reading
	//     for anything written outside a user request.
	//
	// COALESCE keeps the audit-when-possible behavior; the fallback
	// only fires when no real user is on the call.
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO prompts (id, org_id, creator_user_id, name, body, source, allowed_tools, usage_count, created_at, updated_at)
		VALUES ($1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5, $6, 0, now(), now())
	`, p.ID, orgID, p.Name, p.Body, p.Source, p.AllowedTools)
	return err
}

func (s *promptStore) Update(ctx context.Context, orgID string, id, name, body string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompts SET name = $1, body = $2, user_modified = TRUE, updated_at = now()
		WHERE org_id = $3 AND id = $4
	`, name, body, orgID, id)
	return err
}

func (s *promptStore) UpdateImported(ctx context.Context, orgID string, id, name, body, allowedTools string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompts SET name = $1, body = $2, allowed_tools = $3, updated_at = now()
		WHERE org_id = $4 AND id = $5
	`, name, body, allowedTools, orgID, id)
	return err
}

func (s *promptStore) Delete(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `DELETE FROM prompts WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *promptStore) Hide(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `UPDATE prompts SET hidden = TRUE WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *promptStore) Unhide(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `UPDATE prompts SET hidden = FALSE WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *promptStore) IncrementUsage(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompts SET usage_count = usage_count + 1
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return err
}

// --- Stats ---------------------------------------------------------

// Stats mirrors the SQLite impl's three-query shape so the
// conformance harness can assert against identical results across
// both backends. Differences vs SQLite:
//
//   - `DATE(started_at)` becomes `started_at::date` (Postgres has no
//     DATE() function by default; the cast does the same thing).
//   - org_id is included in every WHERE for RLS defense-in-depth.
//
// Like SQLite, the three queries are intentionally separate rather
// than a single CTE — a CTE refactor is a future optimization, not a
// port. If we change it, both backends move together.
func (s *promptStore) Stats(ctx context.Context, orgID string, promptID string) (*db.PromptStats, error) {
	stats := &db.PromptStats{}

	// COALESCE on the SUM(CASE…) columns because SUM over zero rows
	// is NULL in Postgres and *int Scan rejects NULL — the
	// never-used-prompt path otherwise blows up the whole stats panel.
	if err := s.app.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(AVG(total_cost_usd), 0),
			COALESCE(AVG(duration_ms), 0)::bigint,
			COALESCE(SUM(total_cost_usd), 0)
		FROM runs WHERE org_id = $1 AND prompt_id = $2
	`, orgID, promptID).Scan(
		&stats.TotalRuns,
		&stats.CompletedRuns,
		&stats.FailedRuns,
		&stats.AvgCostUSD,
		&stats.AvgDurationMs,
		&stats.TotalCostUSD,
	); err != nil {
		return nil, err
	}
	if stats.TotalRuns > 0 {
		stats.SuccessRate = float64(stats.CompletedRuns) / float64(stats.TotalRuns)
	}

	var lastUsed sql.NullTime
	if err := s.app.QueryRowContext(ctx,
		`SELECT MAX(started_at) FROM runs WHERE org_id = $1 AND prompt_id = $2`, orgID, promptID,
	).Scan(&lastUsed); err != nil {
		log.Printf("[prompt_stats] failed to scan MAX(started_at) for %s: %v", promptID, err)
	}
	if lastUsed.Valid {
		formatted := lastUsed.Time.Format(time.RFC3339)
		stats.LastUsedAt = &formatted
	}

	cutoff := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	rows, err := s.app.QueryContext(ctx, `
		SELECT started_at::date AS day, COUNT(*) AS cnt
		FROM runs
		WHERE org_id = $1 AND prompt_id = $2 AND started_at::date >= $3::date
		GROUP BY day ORDER BY day
	`, orgID, promptID, cutoff)
	if err != nil {
		return stats, nil
	}
	defer rows.Close()

	dayMap := make(map[string]int)
	for rows.Next() {
		var day time.Time
		var cnt int
		if err := rows.Scan(&day, &cnt); err != nil {
			log.Printf("[prompt_stats] failed to scan runs-per-day row for %s: %v", promptID, err)
			continue
		}
		dayMap[day.Format("2006-01-02")] = cnt
	}
	if err := rows.Err(); err != nil {
		log.Printf("[prompt_stats] runs-per-day iteration error for %s: %v", promptID, err)
	}

	for i := 29; i >= 0; i-- {
		d := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		stats.RunsPerDay = append(stats.RunsPerDay, db.DayCount{Date: d, Count: dayMap[d]})
	}
	return stats, nil
}
