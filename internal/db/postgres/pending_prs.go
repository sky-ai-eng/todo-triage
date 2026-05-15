package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pendingPRStore is the Postgres impl of db.PendingPRStore. Wired
// against the app pool in postgres.New: every consumer is
// request-equivalent (pending_prs handler, swipe-dismiss cleanup,
// agent gh-create-pr tool via cmd/exec, spawner goroutine launched
// from a request handler). RLS policy pending_prs_all gates every
// statement via an EXISTS subquery on runs; org_id is also included
// in every WHERE clause as defense in depth.
//
// SQL is written fresh against D3's schema: $N placeholders, org_id
// in every INSERT, native boolean for locked / draft (no boolToInt
// coercion needed), now() for the submit-claim timestamp.
//
// pending_prs is a leaf table — no child rows hang off it — so
// Delete / DeleteByRunID can run a single statement and don't need
// a transaction wrapper. The runs row carries ON DELETE CASCADE so
// pending_prs gets reaped automatically when its parent run is
// removed.
type pendingPRStore struct{ q queryer }

func newPendingPRStore(q queryer) db.PendingPRStore { return &pendingPRStore{q: q} }

var _ db.PendingPRStore = (*pendingPRStore)(nil)

func (s *pendingPRStore) Create(ctx context.Context, orgID string, p domain.PendingPR) error {
	// NULLIF on body so an empty string becomes SQL NULL — matches
	// the SQLite impl's nullIfEmpty(body), which exists so the
	// human-feedback formatter can distinguish "no body captured"
	// (NULL) from "body was a real empty string."
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO pending_prs
		  (id, org_id, run_id, owner, repo, head_branch, head_sha, base_branch,
		   title, body, original_title, original_body, draft)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		        $9, NULLIF($10, ''), $9, NULLIF($10, ''), $11)
	`,
		p.ID, orgID, p.RunID, p.Owner, p.Repo, p.HeadBranch, p.HeadSHA, p.BaseBranch,
		p.Title, p.Body, p.Draft,
	)
	return err
}

func (s *pendingPRStore) Get(ctx context.Context, orgID, id string) (*domain.PendingPR, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE org_id = $1 AND id = $2
	`, orgID, id)
	return pgScanPendingPRRow(row)
}

func (s *pendingPRStore) ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingPR, error) {
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE org_id = $1 AND run_id = $2
	`, orgID, runID)
	return pgScanPendingPRRow(row)
}

func (s *pendingPRStore) UpdateTitleBody(ctx context.Context, orgID, id, title, body string) error {
	res, err := s.q.ExecContext(ctx, `
		UPDATE pending_prs
		   SET title = $1,
		       body = NULLIF($2, ''),
		       original_title = COALESCE(original_title, $1),
		       original_body = COALESCE(original_body, NULLIF($2, ''))
		 WHERE org_id = $3 AND id = $4 AND submitted_at IS NULL
	`, title, body, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Disambiguate "row not found" from "row exists but already
		// submitted" so the caller can give the user the right
		// reason.
		var submittedAt sql.NullTime
		err := s.q.QueryRowContext(ctx,
			`SELECT submitted_at FROM pending_prs WHERE org_id = $1 AND id = $2`,
			orgID, id,
		).Scan(&submittedAt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("pending PR %s not found", id)
			}
			return err
		}
		if submittedAt.Valid {
			return db.ErrPendingPRSubmitted
		}
		return fmt.Errorf("pending PR %s update matched 0 rows but row state is consistent; investigate", id)
	}
	return nil
}

func (s *pendingPRStore) Lock(ctx context.Context, orgID, id, title, body string) error {
	res, err := s.q.ExecContext(ctx, `
		UPDATE pending_prs
		   SET title = $1,
		       body = NULLIF($2, ''),
		       original_title = COALESCE(original_title, $1),
		       original_body = COALESCE(original_body, NULLIF($2, '')),
		       locked = true
		 WHERE org_id = $3 AND id = $4 AND locked = false
	`, title, body, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already queued" from "no such PR" — same
		// SKY-212 disambiguation pattern as ReviewStore.LockSubmission.
		// A bogus id shouldn't get the lock message; the agent should
		// see a different error pointing at the argument.
		var exists int
		if err := s.q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_prs WHERE org_id = $1 AND id = $2`,
			orgID, id,
		).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pending PR %s not found", id)
		}
		return db.ErrPendingPRAlreadyQueued
	}
	return nil
}

func (s *pendingPRStore) MarkSubmitted(ctx context.Context, orgID, id string) (bool, error) {
	// now() is fine here even though it's tx-fixed (vs
	// clock_timestamp()): MarkSubmitted updates a single row per call
	// and the column is a one-shot guard, not an ordering key. Two
	// callers in the same tx wouldn't even reach this point — the
	// `WHERE submitted_at IS NULL` clause only matches once.
	res, err := s.q.ExecContext(ctx, `
		UPDATE pending_prs
		   SET submitted_at = now()
		 WHERE org_id = $1 AND id = $2 AND submitted_at IS NULL
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var exists int
		if qerr := s.q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_prs WHERE org_id = $1 AND id = $2`,
			orgID, id,
		).Scan(&exists); qerr != nil {
			return false, qerr
		}
		if exists == 0 {
			return false, fmt.Errorf("pending PR %s not found", id)
		}
		return false, db.ErrPendingPRSubmitInFlight
	}
	return true, nil
}

func (s *pendingPRStore) ClearSubmitted(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx,
		`UPDATE pending_prs SET submitted_at = NULL WHERE org_id = $1 AND id = $2`,
		orgID, id,
	)
	return err
}

func (s *pendingPRStore) Delete(ctx context.Context, orgID, id string) error {
	res, err := s.q.ExecContext(ctx,
		`DELETE FROM pending_prs WHERE org_id = $1 AND id = $2`, orgID, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending PR %s not found", id)
	}
	return nil
}

func (s *pendingPRStore) DeleteByRunID(ctx context.Context, orgID, runID string) error {
	_, err := s.q.ExecContext(ctx,
		`DELETE FROM pending_prs WHERE org_id = $1 AND run_id = $2`, orgID, runID)
	return err
}

func pgScanPendingPRRow(row *sql.Row) (*domain.PendingPR, error) {
	var p domain.PendingPR
	var origTitle, origBody sql.NullString
	var submittedAt sql.NullTime
	err := row.Scan(
		&p.ID, &p.RunID, &p.Owner, &p.Repo, &p.HeadBranch, &p.HeadSHA, &p.BaseBranch,
		&p.Title, &p.Body,
		&origTitle, &origBody,
		&p.Draft, &p.Locked, &submittedAt, &p.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if origTitle.Valid {
		s := origTitle.String
		p.OriginalTitle = &s
	}
	if origBody.Valid {
		s := origBody.String
		p.OriginalBody = &s
	}
	if submittedAt.Valid {
		t := submittedAt.Time
		p.SubmittedAt = &t
	}
	return &p, nil
}
