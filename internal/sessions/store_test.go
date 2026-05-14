package sessions

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// seedUser inserts the minimum FK chain (auth.users → public.users)
// needed for a sessions row to be insertable. Mirrors the helper in
// pgtest/baseline_test.go but lives here because that helper is
// test-package-private. If a third caller materializes this should
// move into pgtest as an exported helper.
func seedUser(t *testing.T, h *pgtest.Harness) uuid.UUID {
	t.Helper()
	var idStr string
	if err := h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	h.SeedAuthUser(t, idStr, idStr+"@test")
	if _, err := h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, idStr, "test-user"); err != nil {
		t.Fatalf("seed public.users: %v", err)
	}
	return uuid.MustParse(idStr)
}

func newStoreForTest(t *testing.T) (*Store, *pgtest.Harness, uuid.UUID) {
	t.Helper()
	h := pgtest.Shared(t)
	h.Reset(t)
	uid := seedUser(t, h)
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return NewStore(h.AdminDB, k), h, uid
}

func TestStore_CreateLookupRoundtrip(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()

	jwt, refresh := "fake.jwt.token", "fake-refresh-token"
	jwtExp := time.Now().Add(1 * time.Hour).UTC()
	sessExp := time.Now().Add(30 * 24 * time.Hour).UTC()

	created, err := store.Create(ctx, uid, jwt, refresh, jwtExp, sessExp, "test-ua", "127.0.0.1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("Create returned nil id")
	}

	got, err := store.Lookup(ctx, created.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil {
		t.Fatal("Lookup returned nil for existing session")
	}
	if got.JWT != jwt {
		t.Errorf("JWT mismatch: got %q want %q", got.JWT, jwt)
	}
	if got.RefreshToken != refresh {
		t.Errorf("refresh mismatch: got %q want %q", got.RefreshToken, refresh)
	}
	if got.UserID != uid {
		t.Errorf("user_id mismatch: got %s want %s", got.UserID, uid)
	}
	if got.UserAgent != "test-ua" {
		t.Errorf("user_agent: got %q want %q", got.UserAgent, "test-ua")
	}
	if got.IPAddr != "127.0.0.1" {
		t.Errorf("ip_addr: got %q want %q", got.IPAddr, "127.0.0.1")
	}
}

func TestStore_CiphertextAtRest(t *testing.T) {
	// Acceptance bullet: SELECT jwt_enc with the master key absent
	// yields ciphertext, not plaintext.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	plainJWT := "header.payload.signature"
	created, err := store.Create(ctx, uid, plainJWT, "ref",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var jwtEnc []byte
	if err := h.AdminDB.QueryRow(
		`SELECT jwt_enc FROM public.sessions WHERE id = $1`, created.ID,
	).Scan(&jwtEnc); err != nil {
		t.Fatalf("raw select jwt_enc: %v", err)
	}
	if len(jwtEnc) == 0 {
		t.Fatal("jwt_enc empty")
	}
	if string(jwtEnc) == plainJWT {
		t.Fatal("jwt_enc stored as plaintext — encryption not applied")
	}
	// Ensure the JWT bytes don't appear anywhere in the ciphertext as
	// a contiguous substring (loose canary, defends against accidental
	// "encrypt only metadata" bugs).
	for i := 0; i+len(plainJWT) <= len(jwtEnc); i++ {
		if string(jwtEnc[i:i+len(plainJWT)]) == plainJWT {
			t.Fatal("plaintext JWT substring found in stored ciphertext")
		}
	}
}

func TestStore_Lookup_NotFound(t *testing.T) {
	store, _, _ := newStoreForTest(t)
	got, err := store.Lookup(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned non-nil for missing session")
	}
}

func TestStore_Lookup_FiltersRevoked(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, err := store.Lookup(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned revoked session")
	}
}

func TestStore_Lookup_FiltersExpired(t *testing.T) {
	// Acceptance bullet: force-expiry test. Even if jwt_expires_at is
	// still future, expires_at in the past forces re-login.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate the session's outer expiry directly. The CHECK constraint
	// requires expires_at > created_at, so we have to push created_at
	// further into the past to satisfy it. Also push jwt_expires_at
	// (jwt_expires_at <= expires_at).
	if _, err := h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '2 hours',
		       jwt_expires_at = now() - interval '1 hour 30 minutes',
		       expires_at     = now() - interval '1 minute'
		 WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	got, err := store.Lookup(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != nil {
		t.Fatal("Lookup returned expired session")
	}
}

func TestStore_UpdateJWT(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "old-jwt", "old-ref",
		time.Now().Add(1*time.Minute), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	newExp := time.Now().Add(2 * time.Hour).UTC()
	if err := store.UpdateJWT(ctx, c.ID, "new-jwt", "new-ref", newExp); err != nil {
		t.Fatalf("UpdateJWT: %v", err)
	}

	got, err := store.Lookup(ctx, c.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.JWT != "new-jwt" {
		t.Errorf("JWT not rotated: got %q", got.JWT)
	}
	if got.RefreshToken != "new-ref" {
		t.Errorf("refresh not rotated: got %q", got.RefreshToken)
	}
	if got.JWTExpiresAt.Unix() != newExp.Unix() {
		t.Errorf("jwt_expires_at not rotated: got %v want %v", got.JWTExpiresAt, newExp)
	}
}

func TestStore_UpdateJWT_OnRevokedReturnsErr(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	err = store.UpdateJWT(ctx, c.ID, "x", "y", time.Now().Add(1*time.Hour))
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("expected ErrSessionGone, got %v", err)
	}
}

func TestStore_Revoke_PreservesRow(t *testing.T) {
	// Acceptance bullet: logout flips revoked_at; row persists for audit.
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, c.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	var revokedAt sql.NullTime
	if err := h.AdminDB.QueryRow(
		`SELECT revoked_at FROM public.sessions WHERE id = $1`, c.ID,
	).Scan(&revokedAt); err != nil {
		t.Fatalf("post-revoke select: %v", err)
	}
	if !revokedAt.Valid {
		t.Fatal("row revoked but revoked_at is NULL")
	}
}

func TestStore_Revoke_Idempotent(t *testing.T) {
	store, _, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Revoke(ctx, c.ID); err != nil {
		t.Fatalf("Revoke #1: %v", err)
	}
	if err := store.Revoke(ctx, c.ID); err != nil {
		t.Fatalf("Revoke #2: %v", err)
	}
}

func TestStore_TouchLastSeen(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()
	c, err := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Backdate last_seen_at so we can detect the bump.
	if _, err := h.AdminDB.Exec(
		`UPDATE public.sessions SET last_seen_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := store.TouchLastSeen(ctx, c.ID); err != nil {
		t.Fatalf("TouchLastSeen: %v", err)
	}
	var lastSeen time.Time
	if err := h.AdminDB.QueryRow(
		`SELECT last_seen_at FROM public.sessions WHERE id = $1`, c.ID,
	).Scan(&lastSeen); err != nil {
		t.Fatalf("post-touch select: %v", err)
	}
	if time.Since(lastSeen) > 1*time.Minute {
		t.Fatalf("last_seen_at not refreshed: %v ago", time.Since(lastSeen))
	}
}

func TestStore_ReapExpired(t *testing.T) {
	store, h, uid := newStoreForTest(t)
	ctx := context.Background()

	// Three rows:
	//   keep — fresh, non-revoked
	//   reap-rev — revoked 31 days ago (older than retention)
	//   reap-exp — expired 31 days ago, never revoked
	keep, _ := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	reapRev, _ := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")
	reapExp, _ := store.Create(ctx, uid, "j", "r",
		time.Now().Add(1*time.Hour), time.Now().Add(24*time.Hour), "", "")

	if _, err := h.AdminDB.Exec(
		`UPDATE public.sessions SET revoked_at = now() - interval '31 days' WHERE id = $1`, reapRev.ID); err != nil {
		t.Fatalf("backdate revoked_at: %v", err)
	}
	// reapExp: push all three timestamps into the past to satisfy
	// expires_at > created_at and jwt_expires_at <= expires_at.
	if _, err := h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '60 days',
		       jwt_expires_at = now() - interval '32 days',
		       expires_at     = now() - interval '31 days'
		 WHERE id = $1`, reapExp.ID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}

	n, err := store.ReapExpired(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d rows, want 2", n)
	}

	// keep survives
	var c int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM public.sessions WHERE id = $1`, keep.ID,
	).Scan(&c); err != nil {
		t.Fatalf("count keep: %v", err)
	}
	if c != 1 {
		t.Errorf("keep row missing post-reap (count=%d)", c)
	}
}
