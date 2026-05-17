package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestWithSession_LocalShim_InjectsSentinels pins the "handlers read
// identity uniformly via ClaimsFrom/OrgIDFrom in both modes" contract.
// In local mode (TF_MODE=local, authDeps nil) the wrapper must inject
// a synthetic Claims with Subject = LocalDefaultUserID and ctxKeyOrgID
// = LocalDefaultOrgID before delegating. A regression that drops the
// injection would put every handler back into "branch on mode" land —
// every per-handler sweep PR in SKY-253 depends on this.
func TestWithSession_LocalShim_InjectsSentinels(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := &Server{} // authDeps deliberately nil — local-mode boot

	var gotSubject, gotOrgID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := ClaimsFrom(r.Context()); c != nil {
			gotSubject = c.Subject
		}
		gotOrgID = OrgIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	s.withSession(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/api/anything", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (local-mode shim should pass through)", rec.Code)
	}
	if gotSubject != runmode.LocalDefaultUserID {
		t.Errorf("ClaimsFrom().Subject = %q, want %q", gotSubject, runmode.LocalDefaultUserID)
	}
	if gotOrgID != runmode.LocalDefaultOrgID {
		t.Errorf("OrgIDFrom() = %q, want %q", gotOrgID, runmode.LocalDefaultOrgID)
	}
}

// TestHandleMe_LocalMode_Returns401 pins the regression: the shim
// injects sentinel claims into every withSession-wrapped request,
// which would otherwise let /api/me proceed into handleMe's
// Postgres-only body (public.users + tf.current_user_id()) and 500
// against local SQLite. handleMe must gate on runmode and 401 in
// local mode to preserve the pre-shim behavior. The forthcoming
// handler-sweep PR will replace the 401 with a SQLite-compatible
// local identity path returning the sentinel user + sentinel org.
func TestHandleMe_LocalMode_Returns401(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)

	s := &Server{} // authDeps nil → shim injects sentinel claims

	rec := httptest.NewRecorder()
	s.withSession(http.HandlerFunc(s.handleMe)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/me", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (handleMe must short-circuit in local mode to avoid Postgres-only query path)", rec.Code)
	}
}

// TestWithSession_MultiMode_NilAuthDeps_PassesThroughWithoutClaims
// pins the boot-race safety. SetAuthDeps lands after routes() in
// multi mode — a request that races in during that window must NOT
// receive the local-mode sentinel (that would let an unauthenticated
// caller masquerade as the synthetic local user once authDeps lands
// for a different identity model). The correct posture is the prior
// pass-through: handlers see nil claims and write 401 themselves.
func TestWithSession_MultiMode_NilAuthDeps_PassesThroughWithoutClaims(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)

	s := &Server{}

	var sawClaims, sawOrgID bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawClaims = ClaimsFrom(r.Context()) != nil
		sawOrgID = OrgIDFrom(r.Context()) != ""
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	s.withSession(inner).ServeHTTP(rec, httptest.NewRequest("GET", "/api/anything", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (multi-mode pre-deps boot race should pass through)", rec.Code)
	}
	if sawClaims {
		t.Error("ClaimsFrom() returned non-nil in multi mode with nil authDeps; sentinel must NOT bleed across modes")
	}
	if sawOrgID {
		t.Error("OrgIDFrom() returned non-empty in multi mode with nil authDeps; sentinel must NOT bleed across modes")
	}
}
