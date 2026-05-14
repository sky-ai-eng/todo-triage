package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
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
type authDeps struct {
	verifier *verify.Verifier
	sessions *sessions.Store
	// gotrueRefresh is called by SessionMiddleware to perform the
	// refresh-token dance when a JWT is near expiry. Returns the
	// new (accessToken, refreshToken, newJWTExpiresAtUnix).
	//
	// Modeled as a function so the test harness can stub it; the
	// production wiring binds it to a method on *Server that hits
	// the configured gotrue URL.
	gotrueRefresh func(ctx context.Context, refreshToken string) (newJWT string, newRefresh string, jwtExpiresAtUnix int64, err error)
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
		cookie, err := r.Cookie(sessionCookieName)
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
				log.Printf("[auth] refresh failed for sid=%s: %v", sid, err)
				writeUnauth(w)
				return
			}
		}

		claims, err := s.authDeps.verifier.Verify(sess.JWT)
		if err != nil {
			// Either the JWT decrypted cleanly but failed verification
			// (rotated signing key, replay across issuers) — in either
			// case the session is unrecoverable. 401.
			log.Printf("[auth] verify failed for sid=%s: %v", sid, err)
			writeUnauth(w)
			return
		}

		// Best-effort last-seen bump; intentionally backgrounded so
		// the slow DB doesn't lengthen the request critical path.
		// Errors are logged inside the goroutine.
		go func(id uuid.UUID) {
			if err := s.authDeps.sessions.TouchLastSeen(context.Background(), id); err != nil {
				log.Printf("[auth] touch last_seen for sid=%s: %v", id, err)
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

// userHasOrgAccess answers the OrgMiddleware question via a direct
// admin-pool query against org_memberships. Lives on the Server type
// rather than under a store because it predates the per-resource
// store work for org_memberships (D9 will likely move it).
func (s *Server) userHasOrgAccess(ctx context.Context, userID, orgID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM org_memberships
		 WHERE user_id = $1 AND org_id = $2
	`, userID, orgID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
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

// refreshSessionInline runs the refresh dance and re-encrypts the new
// tokens into the session row. The Session struct is updated in place
// so subsequent steps in the middleware see the new JWT.
func (s *Server) refreshSessionInline(ctx context.Context, sess *sessions.Session) error {
	if s.authDeps == nil || s.authDeps.gotrueRefresh == nil {
		return errors.New("refresh not wired")
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

const sessionCookieName = "sid"

func writeUnauth(w http.ResponseWriter) {
	http.Error(w, "unauthenticated", http.StatusUnauthorized)
}
