package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// triggerStore is the Postgres impl of db.TriggerStore. Mirrors the
// SQLite impl's behavior; differences vs SQLite:
//
//   - scope_predicate_json is JSONB — INSERT params cast via $N::jsonb,
//     reads scan via sql.NullString into *string.
//   - org_id NOT NULL: included in every WHERE for RLS defense-in-depth.
//   - creator_user_id NULL for system rows (post-202605120001); the
//     CHECK constraint pairs (source='system' ↔ creator_user_id IS NULL).
//   - team_id / visibility: visibility='org' for system rows (per the
//     same migration); user rows default to 'private' until D-TeamDefault
//     flips the default.
//   - ON CONFLICT (id) DO NOTHING for Seed; prompt_triggers.id is a
//     global PK so per-org UUIDv5 derivation (ShippedPromptTrigger.UUIDFor)
//     prevents cross-tenant collision — same trick TaskRuleStore uses.
//
// # Pool split (mirrors PromptStore + TaskRuleStore)
//
//   - app pool — tf_app, RLS-active. Every CRUD method runs here.
//   - admin pool — supabase_admin, BYPASSRLS. Seed runs here because
//     prompt_triggers_update gates org-visible writes on
//     tf.user_is_org_admin(); boot-time Seed has no claims and would
//     otherwise hit the WITH CHECK on every shipped row.
//
// Inside WithTx both fields point at the same *sql.Tx, and inTx is
// true. Seed inside WithTx is rejected: escaping to the admin pool
// would break the caller's transaction scope (matches PromptStore +
// TaskRuleStore behavior — production seed runs from main.go, outside
// any tx).
type triggerStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newTriggerStore(app, admin queryer) db.TriggerStore {
	return &triggerStore{app: app, admin: admin}
}

// newTxTriggerStore composes a tx-bound TriggerStore for WithTx /
// NewForTx. Both pools collapse onto the caller's tx; inTx=true
// makes Seed refuse rather than silently bypass the tx scope.
func newTxTriggerStore(tx queryer) db.TriggerStore {
	return &triggerStore{app: tx, admin: tx, inTx: true}
}

var _ db.TriggerStore = (*triggerStore)(nil)

const pgTriggerColumns = `id, prompt_id, trigger_type, event_type, scope_predicate_json::text,
       breaker_threshold, min_autonomy_suitability, enabled, source, created_at, updated_at`

func (s *triggerStore) Seed(ctx context.Context, orgID string) error {
	if s.inTx {
		return errors.New("postgres triggers: Seed must not be called inside WithTx; call stores.Triggers.Seed directly")
	}
	now := time.Now().UTC()
	var inserted int64
	for _, t := range db.ShippedPromptTriggers {
		var pred any
		if t.Predicate == "" {
			pred = nil
		} else {
			pred = t.Predicate
		}
		// t.UUIDFor(orgID) — (slug, orgID) → deterministic UUIDv5
		// so each tenant gets its own row id for the same shipped
		// trigger. Same per-org-PK-collision rationale as
		// TaskRuleStore.Seed.
		//
		// admin pool: bypasses RLS so the claims-less boot-time
		// write goes through. CRUD methods below run on app.
		//
		// creator_user_id NULL + visibility='org' + source='system'
		// per the prompt_triggers_system_has_no_creator CHECK from
		// 202605120001. Shipped triggers have no human author; they
		// behave like org-shared rows that any admin can toggle.
		// enabled FALSE per project convention — shipped triggers
		// ship disabled and the user opts in.
		res, err := s.admin.ExecContext(ctx, `
			INSERT INTO prompt_triggers
				(id, org_id, creator_user_id, prompt_id, trigger_type, event_type,
				 scope_predicate_json, breaker_threshold, min_autonomy_suitability,
				 enabled, source, visibility, created_at, updated_at)
			VALUES (
				$1, $2, NULL,
				$3, 'event', $4,
				$5::jsonb, $6, $7,
				FALSE, 'system', 'org', $8, $8
			)
			ON CONFLICT (id) DO NOTHING
		`, t.UUIDFor(orgID), orgID,
			t.PromptID, t.EventType,
			pred, t.BreakerThreshold, t.MinAutonomySuitability,
			now)
		if err != nil {
			return fmt.Errorf("seed prompt_trigger %s: %w", t.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted += n
	}
	log.Printf("[db/pg] seeded %d new prompt_triggers (%d already existed)", inserted, int64(len(db.ShippedPromptTriggers))-inserted)
	return nil
}

func (s *triggerStore) List(ctx context.Context, orgID string) ([]domain.PromptTrigger, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgTriggerColumns+`
		FROM prompt_triggers
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerPG(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *triggerStore) Get(ctx context.Context, orgID string, id string) (*domain.PromptTrigger, error) {
	// prompt_triggers.id is UUID-typed; non-UUID strings would
	// error at the column type layer (22P02) before WHERE evaluates.
	// Treat as not-found to match the SQLite TEXT-keyed semantics.
	// See internal/db/postgres/uuid.go.
	if !isValidUUID(id) {
		return nil, nil
	}
	row := s.app.QueryRowContext(ctx, `
		SELECT `+pgTriggerColumns+`
		FROM prompt_triggers
		WHERE org_id = $1 AND id = $2
	`, orgID, id)
	t, err := scanTriggerRowPG(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *triggerStore) Create(ctx context.Context, orgID string, t domain.PromptTrigger) error {
	if t.TriggerType != domain.TriggerTypeEvent {
		return fmt.Errorf("postgres triggers: unsupported trigger_type %q: only %q is supported", t.TriggerType, domain.TriggerTypeEvent)
	}
	// creator_user_id resolution mirrors PromptStore.Create + TaskRuleStore.Create:
	// prefer the JWT-bound caller, fall back to org owner so system-context
	// writes (tests, future provisioning) still satisfy the NOT NULL FK on
	// user-source rows. (System rows go through Seed which writes NULL.)
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO prompt_triggers
			(id, org_id, creator_user_id, prompt_id, trigger_type, event_type,
			 scope_predicate_json, breaker_threshold, min_autonomy_suitability,
			 enabled, source, created_at, updated_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5,
			$6::jsonb, $7, $8,
			$9, 'user', now(), now()
		)
	`, t.ID, orgID,
		t.PromptID, t.TriggerType, t.EventType,
		pred, t.BreakerThreshold, t.MinAutonomySuitability,
		t.Enabled)
	return err
}

func (s *triggerStore) Update(ctx context.Context, orgID string, t domain.PromptTrigger) error {
	// Invalid UUID → row can't exist; no-op rather than surfacing
	// a Postgres parse error. Production handlers Get-first to 404,
	// so this just makes the mutating path consistent with that.
	if !isValidUUID(t.ID) {
		return nil
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompt_triggers
		SET scope_predicate_json = $1::jsonb, breaker_threshold = $2,
		    min_autonomy_suitability = $3, updated_at = now()
		WHERE org_id = $4 AND id = $5
	`, pred, t.BreakerThreshold, t.MinAutonomySuitability, orgID, t.ID)
	return err
}

func (s *triggerStore) SetEnabled(ctx context.Context, orgID string, id string, enabled bool) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE prompt_triggers SET enabled = $1, updated_at = now() WHERE org_id = $2 AND id = $3
	`, enabled, orgID, id)
	return err
}

func (s *triggerStore) Delete(ctx context.Context, orgID string, id string) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `DELETE FROM prompt_triggers WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *triggerStore) ListForPrompt(ctx context.Context, orgID string, promptID string) ([]domain.PromptTrigger, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgTriggerColumns+`
		FROM prompt_triggers
		WHERE org_id = $1 AND prompt_id = $2
		ORDER BY created_at DESC
	`, orgID, promptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerPG(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func (s *triggerStore) GetActiveForEvent(ctx context.Context, orgID string, eventType string) ([]domain.PromptTrigger, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+pgTriggerColumns+`
		FROM prompt_triggers
		WHERE org_id = $1 AND event_type = $2 AND enabled = TRUE
	`, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []domain.PromptTrigger
	for rows.Next() {
		t, err := scanTriggerPG(rows)
		if err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func scanTriggerPG(rows *sql.Rows) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	var pred sql.NullString
	if err := rows.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &pred,
		&t.BreakerThreshold, &t.MinAutonomySuitability, &t.Enabled, &t.Source, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	if pred.Valid {
		s := pred.String
		t.ScopePredicateJSON = &s
	}
	return t, nil
}

func scanTriggerRowPG(row *sql.Row) (domain.PromptTrigger, error) {
	var t domain.PromptTrigger
	var pred sql.NullString
	if err := row.Scan(&t.ID, &t.PromptID, &t.TriggerType, &t.EventType, &pred,
		&t.BreakerThreshold, &t.MinAutonomySuitability, &t.Enabled, &t.Source, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	if pred.Valid {
		s := pred.String
		t.ScopePredicateJSON = &s
	}
	return t, nil
}
