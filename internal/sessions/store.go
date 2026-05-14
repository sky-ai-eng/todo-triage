package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store wraps the public.sessions table. The DB handle must be the
// admin (BYPASSRLS) pool — session lookup happens *before* we have a
// claim context to install via SET LOCAL request.jwt.claims, so the
// table's RLS policies can't be relied on for the lookup itself.
// (RLS on the table still defends against the eventual app-pool
// reader for the "list my sessions" UI surface in D11.)
type Store struct {
	db  *sql.DB
	key Key
}

// NewStore wires the store. Caller owns the *sql.DB lifecycle; this
// type holds the encryption key by value (32 bytes, cheap to copy).
func NewStore(db *sql.DB, key Key) *Store {
	return &Store{db: db, key: key}
}

// Session is the decrypted-on-read projection. Production rows in
// public.sessions hold ciphertext for jwt + refresh; this struct holds
// them already-decrypted.
type Session struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	JWT          string
	RefreshToken string
	JWTExpiresAt time.Time
	ExpiresAt    time.Time
	CreatedAt    time.Time
	LastSeenAt   time.Time
	RevokedAt    sql.NullTime
	UserAgent    string
	IPAddr       string
}

// Create encrypts the tokens and inserts a fresh row. Returns the
// session ID (the opaque value we'll set on the sid cookie).
func (s *Store) Create(
	ctx context.Context,
	userID uuid.UUID,
	jwt, refresh string,
	jwtExp, sessExp time.Time,
	userAgent, ipAddr string,
) (*Session, error) {
	jwtEnc, jwtNonce, err := s.key.Encrypt([]byte(jwt))
	if err != nil {
		return nil, fmt.Errorf("encrypt jwt: %w", err)
	}
	refEnc, refNonce, err := s.key.Encrypt([]byte(refresh))
	if err != nil {
		return nil, fmt.Errorf("encrypt refresh: %w", err)
	}

	// CHECK constraint on the table: jwt_expires_at <= expires_at. We
	// could clamp here, but a violation almost certainly means a caller
	// bug (gotrue's session expiry shorter than its JWT? — wrong) so
	// surfacing the constraint error is more useful than silently
	// shifting the timestamps.
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO public.sessions
		    (user_id, jwt_enc, jwt_nonce, refresh_token_enc, refresh_nonce,
		     jwt_expires_at, expires_at, user_agent, ip_addr)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, '')::inet)
		RETURNING id, created_at, last_seen_at
	`, userID, jwtEnc, jwtNonce, refEnc, refNonce, jwtExp, sessExp, userAgent, ipAddr)

	out := &Session{
		UserID:       userID,
		JWT:          jwt,
		RefreshToken: refresh,
		JWTExpiresAt: jwtExp,
		ExpiresAt:    sessExp,
		UserAgent:    userAgent,
		IPAddr:       ipAddr,
	}
	if err := row.Scan(&out.ID, &out.CreatedAt, &out.LastSeenAt); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return out, nil
}

// Lookup fetches a non-revoked, non-expired session by id, decrypts its
// tokens, and returns it. Returns (nil, nil) when no matching row exists
// — the caller renders 401. An error return means a DB or decrypt
// failure, which is a 500.
//
// The decrypt-on-miss-key case (TF_SESSION_KEY rotated, existing rows
// can no longer be decrypted) deliberately surfaces as an error rather
// than (nil, nil): callers shouldn't quietly treat "session existed but
// we can't read it" the same as "no session." Rotation of the master
// key requires explicit handling (drain sessions, re-issue).
func (s *Store) Lookup(ctx context.Context, sid uuid.UUID) (*Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id,
		       jwt_enc, jwt_nonce, refresh_token_enc, refresh_nonce,
		       jwt_expires_at, expires_at, created_at, last_seen_at, revoked_at,
		       COALESCE(user_agent, ''), COALESCE(host(ip_addr), '')
		  FROM public.sessions
		 WHERE id = $1
		   AND revoked_at IS NULL
		   AND expires_at > now()
	`, sid)

	var (
		out                                Session
		jwtEnc, jwtNonce, refEnc, refNonce []byte
	)
	err := row.Scan(
		&out.ID, &out.UserID,
		&jwtEnc, &jwtNonce, &refEnc, &refNonce,
		&out.JWTExpiresAt, &out.ExpiresAt, &out.CreatedAt, &out.LastSeenAt, &out.RevokedAt,
		&out.UserAgent, &out.IPAddr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query session: %w", err)
	}

	jwtBytes, err := s.key.Decrypt(jwtEnc, jwtNonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt jwt for session %s: %w", sid, err)
	}
	refBytes, err := s.key.Decrypt(refEnc, refNonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh for session %s: %w", sid, err)
	}
	out.JWT = string(jwtBytes)
	out.RefreshToken = string(refBytes)
	return &out, nil
}

// UpdateJWT rewrites the access + refresh tokens after a successful
// refresh dance with GoTrue. Does not touch expires_at — the session's
// own 30-day window keeps ticking; only the inner JWT/refresh rotate.
func (s *Store) UpdateJWT(
	ctx context.Context,
	sid uuid.UUID,
	jwt, refresh string,
	jwtExp time.Time,
) error {
	jwtEnc, jwtNonce, err := s.key.Encrypt([]byte(jwt))
	if err != nil {
		return fmt.Errorf("encrypt jwt: %w", err)
	}
	refEnc, refNonce, err := s.key.Encrypt([]byte(refresh))
	if err != nil {
		return fmt.Errorf("encrypt refresh: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE public.sessions
		   SET jwt_enc = $2, jwt_nonce = $3,
		       refresh_token_enc = $4, refresh_nonce = $5,
		       jwt_expires_at = $6
		 WHERE id = $1
		   AND revoked_at IS NULL
	`, sid, jwtEnc, jwtNonce, refEnc, refNonce, jwtExp)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Session vanished or was revoked mid-refresh; signal caller
		// to drop the cookie rather than silently no-op.
		return ErrSessionGone
	}
	return nil
}

// ErrSessionGone signals that a session we held a handle to was deleted
// or revoked between read and write. Middleware turns this into a 401.
var ErrSessionGone = errors.New("session no longer exists or was revoked")

// Revoke flips revoked_at to now() on a session. Soft-delete: row stays
// for audit. Idempotent — calling on an already-revoked row is a no-op.
func (s *Store) Revoke(ctx context.Context, sid uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE public.sessions
		   SET revoked_at = now()
		 WHERE id = $1
		   AND revoked_at IS NULL
	`, sid)
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// TouchLastSeen bumps last_seen_at to now(). Best-effort — errors are
// swallowed by the caller (middleware fires this in a goroutine).
func (s *Store) TouchLastSeen(ctx context.Context, sid uuid.UUID) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE public.sessions SET last_seen_at = now() WHERE id = $1
	`, sid)
	if err != nil {
		return fmt.Errorf("touch last_seen: %w", err)
	}
	return nil
}

// ReapExpired hard-deletes:
//   - revoked rows older than retention (logged out long ago — audit window passed)
//   - naturally-expired rows older than retention (sat unrenewed past the session window)
//
// Returns total rows deleted across both categories.
func (s *Store) ReapExpired(ctx context.Context, retention time.Duration) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM public.sessions
		 WHERE (revoked_at IS NOT NULL AND revoked_at <  now() - $1::interval)
		    OR (revoked_at IS NULL     AND expires_at <  now() - $1::interval)
	`, retention.String())
	if err != nil {
		return 0, fmt.Errorf("reap sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
