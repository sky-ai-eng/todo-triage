package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// chainStore is the Postgres impl of db.ChainStore.
//
// org_id is threaded through every WHERE for defense in depth; RLS
// enforces the creator predicate on chain_runs and prompt_chain_steps
// RLS gates on the parent prompt's visibility.
//
// Reads against UUID-typed columns (chain_runs.id, runs.id, …) guard
// inputs with isValidUUID and treat non-UUID strings as not-found —
// otherwise Postgres surfaces 22P02 at the column-type layer, which
// doesn't match the SQLite TEXT-keyed semantics callers expect.
//
// No admin/app pool split: every method runs on the app pool. There's
// no boot-time seed (chain rows are user-created), so the claims-less
// write path other stores need doesn't apply.
type chainStore struct{ app queryer }

func newChainStore(app queryer) db.ChainStore { return &chainStore{app: app} }

var _ db.ChainStore = (*chainStore)(nil)

func (s *chainStore) ListSteps(ctx context.Context, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT chain_prompt_id, step_index, step_prompt_id, brief, created_at
		FROM prompt_chain_steps
		WHERE org_id = $1 AND chain_prompt_id = $2
		ORDER BY step_index ASC
	`, orgID, chainPromptID)
	if err != nil {
		return nil, fmt.Errorf("query chain steps: %w", err)
	}
	defer rows.Close()

	var out []domain.ChainStep
	for rows.Next() {
		var st domain.ChainStep
		if err := rows.Scan(&st.ChainPromptID, &st.StepIndex, &st.StepPromptID, &st.Brief, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *chainStore) CountStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error) {
	var n int
	err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT chain_prompt_id)
		FROM prompt_chain_steps
		WHERE org_id = $1 AND step_prompt_id = $2
	`, orgID, stepPromptID).Scan(&n)
	return n, err
}

func (s *chainStore) ReplaceSteps(ctx context.Context, orgID, chainPromptID string, stepPromptIDs, briefs []string) error {
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return s.runInTx(ctx, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM prompt_chain_steps WHERE org_id = $1 AND chain_prompt_id = $2`,
			orgID, chainPromptID); err != nil {
			return fmt.Errorf("clear existing steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO prompt_chain_steps (org_id, chain_prompt_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, orgID, chainPromptID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
		return nil
	})
}

func (s *chainStore) CreateRun(ctx context.Context, orgID string, cr domain.ChainRun) (string, error) {
	if cr.ID == "" {
		cr.ID = uuid.New().String()
	}
	if cr.Status == "" {
		cr.Status = domain.ChainRunStatusRunning
	}
	if cr.TriggerType == "" {
		return "", errors.New("chain run trigger type required")
	}
	var triggerID any
	if cr.TriggerID != "" {
		triggerID = cr.TriggerID
	}
	var abortReason any
	if cr.AbortReason != "" {
		abortReason = cr.AbortReason
	}
	var completedAt any
	if cr.CompletedAt != nil {
		completedAt = cr.CompletedAt.UTC()
	}
	// creator_user_id resolution mirrors PromptStore / TaskRuleStore /
	// TriggerStore: prefer the JWT-bound caller, fall back to org owner
	// so system-context writes still satisfy the NOT NULL FK.
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO chain_runs
			(id, org_id, creator_user_id, chain_prompt_id, task_id, trigger_type, trigger_id,
			 status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5, $6,
			$7, $8, $9, $10, now()
		)
	`, cr.ID, orgID, cr.ChainPromptID, cr.TaskID, cr.TriggerType, triggerID, cr.Status, cr.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert chain_run: %w", err)
	}
	return cr.ID, nil
}

func (s *chainStore) GetRun(ctx context.Context, orgID, id string) (*domain.ChainRun, error) {
	// chain_runs.id is UUID-typed; non-UUID strings would error at the
	// column type layer (22P02) before WHERE evaluates. Treat as
	// not-found to match the SQLite TEXT-keyed semantics.
	if !isValidUUID(id) {
		return nil, nil
	}
	var (
		cr            domain.ChainRun
		triggerID     sql.NullString
		abortReason   sql.NullString
		abortedAtStep sql.NullInt64
		completedAt   sql.NullTime
	)
	err := s.app.QueryRowContext(ctx, `
		SELECT id, chain_prompt_id, task_id, trigger_type, trigger_id, status,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM chain_runs WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan(&cr.ID, &cr.ChainPromptID, &cr.TaskID, &cr.TriggerType, &triggerID, &cr.Status,
		&abortReason, &abortedAtStep, &cr.WorktreePath, &cr.StartedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if triggerID.Valid {
		cr.TriggerID = triggerID.String
	}
	if abortReason.Valid {
		cr.AbortReason = abortReason.String
	}
	if abortedAtStep.Valid {
		i := int(abortedAtStep.Int64)
		cr.AbortedAtStep = &i
	}
	if completedAt.Valid {
		t := completedAt.Time
		cr.CompletedAt = &t
	}
	return &cr, nil
}

func (s *chainStore) GetRunForRun(ctx context.Context, orgID, runID string) (*domain.ChainRun, *int, error) {
	if !isValidUUID(runID) {
		return nil, nil, nil
	}
	var (
		chainRunID sql.NullString
		stepIndex  sql.NullInt64
	)
	err := s.app.QueryRowContext(ctx,
		`SELECT chain_run_id, chain_step_index FROM runs WHERE org_id = $1 AND id = $2`,
		orgID, runID).Scan(&chainRunID, &stepIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !chainRunID.Valid {
		return nil, nil, nil
	}
	cr, err := s.GetRun(ctx, orgID, chainRunID.String)
	if err != nil || cr == nil {
		return nil, nil, err
	}
	var idx *int
	if stepIndex.Valid {
		v := int(stepIndex.Int64)
		idx = &v
	}
	return cr, idx, nil
}

func (s *chainStore) MarkRunStatus(ctx context.Context, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	if !isValidUUID(id) {
		return false, nil
	}
	var (
		reasonArg any
		stepArg   any
	)
	if abortReason != "" {
		reasonArg = abortReason
	}
	if abortedAtStep != nil {
		stepArg = *abortedAtStep
	}
	res, err := s.app.ExecContext(ctx, `
		UPDATE chain_runs
		SET status = $1, abort_reason = $2, aborted_at_step = $3, completed_at = now()
		WHERE org_id = $4 AND id = $5
		  AND status IN ('running','pending_approval','awaiting_input')
	`, string(status), reasonArg, stepArg, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *chainStore) RunsForChain(ctx context.Context, orgID, chainRunID string) ([]domain.AgentRun, error) {
	if !isValidUUID(chainRunID) {
		return nil, nil
	}
	rows, err := s.app.QueryContext(ctx, `
		SELECT id, task_id, prompt_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, chain_run_id, chain_step_index
		FROM runs
		WHERE org_id = $1 AND chain_run_id = $2
		ORDER BY chain_step_index ASC, started_at ASC
	`, orgID, chainRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AgentRun
	for rows.Next() {
		var (
			r           domain.AgentRun
			completedAt sql.NullTime
			costUSD     sql.NullFloat64
			durationMs  sql.NullInt64
			numTurns    sql.NullInt64
			chainStep   sql.NullInt64
			promptID    sql.NullString
			model       sql.NullString
			stopReason  sql.NullString
			worktreeP   sql.NullString
			resultSum   sql.NullString
			sessionID   sql.NullString
			chainRunIDs sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.TaskID, &promptID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &stopReason, &worktreeP, &resultSum, &sessionID,
			&chainRunIDs, &chainStep); err != nil {
			return nil, err
		}
		r.PromptID = promptID.String
		r.Model = model.String
		r.StopReason = stopReason.String
		r.WorktreePath = worktreeP.String
		r.ResultSummary = resultSum.String
		r.SessionID = sessionID.String
		if chainRunIDs.Valid {
			r.ChainRunID = chainRunIDs.String
		}
		if chainStep.Valid {
			v := int(chainStep.Int64)
			r.ChainStepIndex = &v
		}
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *chainStore) InsertVerdict(ctx context.Context, orgID, runID, metadataJSON string) error {
	if !isValidUUID(runID) {
		return fmt.Errorf("postgres chains: invalid run_id %q", runID)
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO run_artifacts (id, org_id, run_id, kind, metadata_json, is_primary, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'chain:verdict', $3::jsonb, FALSE, now())
	`, orgID, runID, metadataJSON)
	return err
}

func (s *chainStore) GetLatestVerdict(ctx context.Context, orgID, runID string) (*domain.ChainVerdict, error) {
	if !isValidUUID(runID) {
		return nil, nil
	}
	var raw sql.NullString
	err := s.app.QueryRowContext(ctx, `
		SELECT metadata_json::text FROM run_artifacts
		WHERE run_id = $1 AND kind = 'chain:verdict'
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return &domain.ChainVerdict{}, nil
	}
	var v domain.ChainVerdict
	if err := json.Unmarshal([]byte(raw.String), &v); err != nil {
		return nil, fmt.Errorf("decode verdict: %w", err)
	}
	return &v, nil
}

func (s *chainStore) LatestVerdictsForRuns(ctx context.Context, orgID string, runIDs []string) (map[string]*domain.ChainVerdict, error) {
	if len(runIDs) == 0 {
		return map[string]*domain.ChainVerdict{}, nil
	}
	// Drop non-UUID ids up front: passing them as parameters to a
	// UUID-typed column would 22P02. Mirrors the per-id isValidUUID
	// guards on the single-key reads above.
	valid := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		if isValidUUID(id) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return map[string]*domain.ChainVerdict{}, nil
	}

	placeholders := make([]string, len(valid))
	args := make([]any, len(valid))
	for i, id := range valid {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT run_id, metadata_json::text
		FROM (
			SELECT run_id, metadata_json,
			       ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY created_at DESC, id DESC) AS rn
			FROM run_artifacts
			WHERE run_id IN (%s) AND kind = 'chain:verdict'
		) ranked
		WHERE rn = 1
	`, strings.Join(placeholders, ","))

	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest verdicts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]*domain.ChainVerdict)
	for rows.Next() {
		var (
			runID string
			raw   sql.NullString
		)
		if err := rows.Scan(&runID, &raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			out[runID] = &domain.ChainVerdict{}
			continue
		}
		var v domain.ChainVerdict
		if err := json.Unmarshal([]byte(raw.String), &v); err != nil {
			return nil, fmt.Errorf("decode verdict for run %s: %w", runID, err)
		}
		out[runID] = &v
	}
	return out, rows.Err()
}

// runInTx is the multi-statement helper for ReplaceSteps. Composes with
// the caller's *sql.Tx inside WithTx so the DELETE + per-step INSERTs
// land atomically alongside any outer write; otherwise opens a fresh tx
// on the app pool. Same shape taskRuleStore.runInTx uses.
func (s *chainStore) runInTx(ctx context.Context, fn func(queryer) error) error {
	switch v := s.app.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres chains: unexpected queryer type")
	}
}
