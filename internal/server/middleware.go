package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	tfdb "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// timeNow is package-var so middleware tests can stub the clock.
// Production callers use time.Now() via this seam.
var timeNow = time.Now

func unixToTime(unixSeconds int64) time.Time {
	return time.Unix(unixSeconds, 0).UTC()
}

// Request-context keys. Unexported type so callers must use the
// exported accessors below — prevents accidental shadowing.
type ctxKey int

const (
	ctxKeyClaims ctxKey = iota
	ctxKeySession
	ctxKeyOrgID
)

// ClaimsFrom returns the verified JWT claims set by SessionMiddleware,
// or nil if the request didn't pass through it. Handlers that depend
// on a claim should fail closed on nil; the middleware would have
// already rejected an unauthenticated request, so nil from this
// helper inside a protected handler indicates a registration bug.
func ClaimsFrom(ctx context.Context) *verify.Claims {
	v, _ := ctx.Value(ctxKeyClaims).(*verify.Claims)
	return v
}

// SessionFrom returns the resolved session row. Used by /api/auth/logout
// to know which sid to revoke without re-reading the cookie.
func SessionFrom(ctx context.Context) *sessions.Session {
	v, _ := ctx.Value(ctxKeySession).(*sessions.Session)
	return v
}

// OrgIDFrom returns the URL-path org_id that OrgMiddleware validated
// against the caller's memberships. Empty string for routes without
// {org_id} or in local mode.
func OrgIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrgID).(string)
	return v
}

// authDeps groups the auth-stack dependencies a Server is wired with.
// Held by-pointer on the Server so a nil group cleanly signals
// "local mode, no auth surface" without scattering individual nil
// checks across every middleware/handler.
//
// The three gotrue* functions are abstracted as closures (not methods)
// so the test harness can stub each independently — the integration
// tests don't run a real gotrue, so the production HTTP calls become
// in-process stubs that return canned shapes.
type authDeps struct {
	verifier *verify.Verifier
	sessions *sessions.Store

	// gotrueRefresh performs the refresh-token dance when a JWT is
	// near expiry. Returns (newJWT, newRefresh, jwtExpiresAtUnix).
	gotrueRefresh func(ctx context.Context, refreshToken string) (newJWT string, newRefresh string, jwtExpiresAtUnix int64, err error)

	// gotrueExchange performs the PKCE auth-code exchange after the
	// provider dance. Returns (accessToken, refreshToken,
	// jwtExpiresAtUnix). Called from handleOAuthCallback.
	gotrueExchange func(ctx context.Context, authCode, codeVerifier string) (accessToken string, refreshToken string, jwtExpiresAtUnix int64, err error)

	// gotrueLogout asks gotrue to invalidate the refresh-token family
	// upstream. Called from handleLogout as best-effort — if it fails
	// we still revoke the row locally and clear the cookie.
	gotrueLogout func(ctx context.Context, accessToken string) error
}

// withSession wraps a handler in the session middleware. The check for
// authDeps==nil happens at REQUEST TIME (not at wrap time) because
// SetAuthDeps is called after routes() registers handlers — capturing
// nil at wrap time would leave the wrapper inert for the entire process
// lifetime even after deps land.
//
// Local-mode behavior: when authDeps stays nil, the wrapper passes the
// request through without setting any claim in context. Downstream
// handlers (/api/me) detect the missing claim and write 401, which is
// the right answer for a local-mode caller hitting a multi-mode-only
// endpoint.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authDeps == nil {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(s.sidCookieName())
		if err != nil {
			writeUnauth(w)
			return
		}
		sid, err := uuid.Parse(cookie.Value)
		if err != nil {
			writeUnauth(w)
			return
		}

		sess, err := s.authDeps.sessions.Lookup(r.Context(), sid)
		if err != nil {
			log.Printf("[auth] session lookup: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if sess == nil {
			writeUnauth(w)
			return
		}

		// Refresh inline if the JWT is within the refresh window
		// (60s). Failing the refresh forces re-login — better to
		// surface the failure now than to verify against an
		// already-expired JWT and 401 anyway.
		if needsRefresh(sess) {
			if err := s.refreshSessionInline(r.Context(), sess); err != nil {
				log.Printf("[auth] refresh failed for sid=%s: %v", sessions.LogID(sid), err)
				writeUnauth(w)
				return
			}
		}

		claims, err := s.authDeps.verifier.Verify(sess.JWT)
		if err != nil {
			// Either the JWT decrypted cleanly but failed verification
			// (rotated signing key, replay across issuers) — in either
			// case the session is unrecoverable. 401.
			log.Printf("[auth] verify failed for sid=%s: %v", sessions.LogID(sid), err)
			writeUnauth(w)
			return
		}

		// Best-effort last-seen bump; intentionally backgrounded so
		// the slow DB doesn't lengthen the request critical path.
		// Errors are logged inside the goroutine.
		go func(id uuid.UUID) {
			if err := s.authDeps.sessions.TouchLastSeen(context.Background(), id); err != nil {
				log.Printf("[auth] touch last_seen for sid=%s: %v", sessions.LogID(id), err)
			}
		}(sid)

		ctx := context.WithValue(r.Context(), ctxKeyClaims, claims)
		ctx = context.WithValue(ctx, ctxKeySession, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withOrg wraps a handler in the org-membership check. Reads
// r.PathValue("org_id"), confirms the caller is a member, and 404s
// otherwise (404 not 403 — don't leak the org's existence).
//
// Must be composed *after* withSession; uses ClaimsFrom to read the
// caller's sub. Routes without {org_id} in the pattern pass through
// unchanged.
func (s *Server) withOrg(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgID := r.PathValue("org_id")
		if orgID == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Cheap validation before the DB hit — malformed UUID in the
		// path is a 404 (same response as "no such org").
		if _, err := uuid.Parse(orgID); err != nil {
			http.NotFound(w, r)
			return
		}
		claims := ClaimsFrom(r.Context())
		if claims == nil {
			// Programmer error: withOrg mounted without withSession.
			// Don't reveal the misconfiguration to the caller.
			log.Printf("[auth] withOrg saw no claims — route missing withSession wrapper: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		ok, err := s.userHasOrgAccess(r.Context(), claims.Subject, orgID)
		if err != nil {
			log.Printf("[auth] membership check %s/%s: %v", claims.Subject, orgID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyOrgID, orgID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// userHasOrgAccess answers the OrgMiddleware question by delegating to
// the tf.user_has_org_access SQL helper, which internally reads
// request.jwt.claims via tf.current_user_id(). The claims-context
// transaction means a missing/wrong claim → NULL → no membership,
// even if a future bug allowed a wrong userID argument to land here.
// Once D9 wires the app pool, the same query runs under RLS without
// further edits.
func (s *Server) userHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
	var ok bool
	err := tfdb.WithTx(ctx, s.db, tfdb.Claims{Sub: userID},
		func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx,
				`SELECT tf.user_has_org_access($1::uuid)`, orgID,
			).Scan(&ok)
		},
	)
	return ok, err
}

// needsRefresh is true when the JWT will expire within the refresh
// window (60s). Keeps the threshold in one place; tests can shadow it.
func needsRefresh(sess *sessions.Session) bool {
	const refreshWindowSeconds = 60
	return sess.JWTExpiresAt.Unix()-nowUnix() < refreshWindowSeconds
}

// nowUnix is var-able so tests can shift the clock.
var nowUnix = func() int64 {
	return timeNow().Unix()
}

// refreshSessionInline runs the refresh dance under a per-session
// mutex. The lock + double-checked re-fetch is the safe pattern:
//
//  1. Request A and B both Lookup before either acquires the lock.
//     Both see a session that needs refresh.
//  2. Request A wins the lock race, calls gotrue, gets new tokens,
//     persists them, releases the lock.
//  3. Request B acquires the lock, re-fetches the session (now fresh
//     in the DB), and sees needsRefresh == false. It skips the
//     refresh call and proceeds with the new JWT.
//
// Without the re-fetch + re-check, Request B would call gotrue with
// the now-rotated refresh token and fail — GoTrue's refresh-token-
// family rotation invalidates the original on use.
//
// The Session struct passed in is mutated in place to point at the
// fresh JWT so subsequent middleware steps see it.
func (s *Server) refreshSessionInline(ctx context.Context, sess *sessions.Session) error {
	if s.authDeps == nil || s.authDeps.gotrueRefresh == nil {
		return errors.New("refresh not wired")
	}

	unlock := s.acquireRefreshLock(sess.ID)
	defer unlock()

	// Re-fetch under the lock. Whoever won the race may have already
	// refreshed; we must observe their work before deciding to call
	// gotrue ourselves.
	fresh, err := s.authDeps.sessions.Lookup(ctx, sess.ID)
	if err != nil {
		return fmt.Errorf("re-fetch session: %w", err)
	}
	if fresh == nil {
		return errors.New("session revoked during refresh wait")
	}
	*sess = *fresh

	if !needsRefresh(sess) {
		// The race winner already refreshed. We have the fresh JWT;
		// nothing to do.
		return nil
	}

	newJWT, newRefresh, newExp, err := s.authDeps.gotrueRefresh(ctx, sess.RefreshToken)
	if err != nil {
		return err
	}
	newExpTime := unixToTime(newExp)
	if err := s.authDeps.sessions.UpdateJWT(ctx, sess.ID, newJWT, newRefresh, newExpTime); err != nil {
		return err
	}
	sess.JWT = newJWT
	sess.RefreshToken = newRefresh
	sess.JWTExpiresAt = newExpTime
	return nil
}

// acquireRefreshLock returns a function that unlocks the per-session
// mutex. The mutex is created on first use via LoadOrStore — concurrent
// first-callers see the same mutex thanks to sync.Map's atomicity.
func (s *Server) acquireRefreshLock(sid uuid.UUID) func() {
	v, _ := s.refreshLocks.LoadOrStore(sid, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// withCSRFOriginCheck rejects mutating requests (POST/PUT/PATCH/DELETE)
// whose Origin header doesn't match the configured publicURL. Browsers
// always send Origin on cross-origin requests, so this catches the
// gap that SameSite=Lax leaves (which permits top-level cross-site
// POSTs to the request URL).
//
// Local mode (authCfg nil): pass-through. Local mode doesn't expose
// session-cookie auth, so there's no CSRF surface to defend.
//
// Same-origin requests that omit Origin (rare; some old browsers,
// fetch() in non-CORS modes) are allowed: a missing Origin can't
// indicate cross-site since cross-site mutating requests must set it.
func (s *Server) withCSRFOriginCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authCfg == nil {
			next.ServeHTTP(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			// No Origin → not a cross-site browser request. Allow.
			// (Server-to-server or curl without Origin lands here;
			// those callers don't have CSRF as an attack vector
			// since they're not cookie-authed-against-their-will.)
			next.ServeHTTP(w, r)
			return
		}
		if origin != s.authCfg.publicURL {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sidCookieName resolves at request time: __Host-sid for HTTPS
// deployments (browser-enforced: Secure flag required, Path=/, no
// Domain), plain sid otherwise. Local-dev / tests run over HTTP and
// would have the browser silently drop a __Host- cookie that doesn't
// also carry Secure.
func (s *Server) sidCookieName() string {
	if s.authCfg != nil && s.authCfg.secureCookies {
		return "__Host-sid"
	}
	return "sid"
}

// cookieSecure derives whether to mark a cookie Secure. True if the
// deployment is HTTPS (publicURL starts with https://) OR the
// individual request arrived over TLS. The latter covers reverse-
// proxy deployments where TLS termination happens upstream and the
// Go server sees plain HTTP — X-Forwarded-Proto = https.
func (s *Server) cookieSecure(r *http.Request) bool {
	if s.authCfg != nil && s.authCfg.secureCookies {
		return true
	}
	return isHTTPS(r)
}

func writeUnauth(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}
