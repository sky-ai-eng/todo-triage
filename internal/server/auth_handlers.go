package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// authConfig holds the multi-mode auth flow's configuration. Loaded
// once at startup and passed to SetAuthDeps; everything in here is
// publicly resolvable (URLs) or only-meaningful inside our own
// signing — none of it is a credential the runtime should re-fetch.
type authConfig struct {
	// gotrueURL is the in-network base URL (e.g. http://gotrue:9999)
	// for server-side calls — token exchange, refresh, etc. Distinct
	// from publicURL because docker-compose internal hostnames don't
	// resolve from the browser side.
	gotrueURL string

	// publicURL is the externally-visible base for the TF deployment
	// (e.g. https://triagefactory.acme.com). Used to construct
	// browser-facing redirect URLs (gotrue's /authorize knows our
	// /api/auth/callback path via the `redirect_to` query param).
	publicURL string

	// stateKey signs the short-lived state cookie that carries
	// return_to + CSRF through the OAuth roundtrip. Derived from
	// TF_SESSION_KEY via HMAC-SHA256 with domain separation, so the
	// cookie-signing subkey and the AES-GCM master never share
	// material directly.
	stateKey [32]byte
}

// SetAuthDeps wires the multi-mode auth dependencies into the server.
// Local mode never calls this; multi-mode boot calls it once after
// constructing the verifier + session store and before ListenAndServe.
//
// Also builds the /auth/v1/* reverse proxy and stashes it on the
// server (read by the mux handler registered in routes()).
func (s *Server) SetAuthDeps(
	verifier *verify.Verifier,
	sessionStore *sessions.Store,
	gotrueURL, publicURL string,
	masterKey [32]byte,
) error {
	cfg := &authConfig{
		gotrueURL: strings.TrimRight(gotrueURL, "/"),
		publicURL: strings.TrimRight(publicURL, "/"),
	}
	deriveStateKey(masterKey, &cfg.stateKey)

	proxy, err := newGotrueProxy(cfg.gotrueURL)
	if err != nil {
		return fmt.Errorf("gotrue proxy: %w", err)
	}

	s.authCfg = cfg
	s.authProxy = proxy
	s.authDeps = &authDeps{
		verifier:      verifier,
		sessions:      sessionStore,
		gotrueRefresh: s.gotrueRefreshFunc(cfg),
	}
	return nil
}

// deriveStateKey produces a subkey for the state cookie HMAC. Domain
// separation via a fixed label so a future need for a second subkey
// (e.g. for CSRF or pre-auth telemetry) doesn't collide with this one.
func deriveStateKey(master [32]byte, out *[32]byte) {
	mac := hmac.New(sha256.New, master[:])
	mac.Write([]byte("triagefactory:auth:state:v1"))
	copy(out[:], mac.Sum(nil))
}

// handleOAuthStart redirects the browser to gotrue's /authorize. The
// `return_to` query param is stashed in a short-lived signed cookie so
// the callback handler can hop back to the right SPA route afterward.
//
// GET /api/auth/oauth/{provider}?return_to=/some/path
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}
	provider := r.PathValue("provider")
	if provider != "github" {
		http.Error(w, "unsupported provider", http.StatusNotFound)
		return
	}

	returnTo := normalizeReturnTo(r.URL.Query().Get("return_to"))

	csrfRaw := make([]byte, 16)
	if _, err := rand.Read(csrfRaw); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	state := stateClaims{
		ReturnTo:  returnTo,
		CSRF:      hex.EncodeToString(csrfRaw),
		ExpiresAt: timeNow().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(s.authCfg.stateKey)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    signed,
		Path:     "/api/auth/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	q := url.Values{}
	q.Set("provider", provider)
	q.Set("redirect_to", s.authCfg.publicURL+"/api/auth/callback?state="+state.CSRF)
	target := s.authCfg.publicURL + "/auth/v1/authorize?" + q.Encode()

	http.Redirect(w, r, target, http.StatusFound)
}

// handleOAuthCallback completes the dance: validates state, accepts
// access_token + refresh_token + expires_in from query params, verifies
// the JWT, upserts public.users, creates an encrypted session, sets
// the sid cookie, and redirects to return_to.
//
// Token delivery: GoTrue's default browser flow stuffs tokens into the
// URL fragment, which doesn't reach servers. D8 ships a tiny SPA
// bootstrap that catches the fragment and re-issues the redirect as
// query-string parameters that this handler accepts. For integration
// tests (D7), the test harness drives this handler directly with the
// expected query-string shape — equivalent surface, simpler test rig.
//
// GET /api/auth/callback?state=<csrf>&access_token=...&refresh_token=...&expires_in=...
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	state, err := parseStateCookie(stateCookie.Value, s.authCfg.stateKey)
	if err != nil {
		log.Printf("[auth] state cookie: %v", err)
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != state.CSRF {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}

	// State done — clear the cookie.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/api/auth/",
		MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})

	accessToken := r.URL.Query().Get("access_token")
	refreshToken := r.URL.Query().Get("refresh_token")
	expiresInStr := r.URL.Query().Get("expires_in")
	if accessToken == "" || refreshToken == "" {
		http.Error(w, "missing tokens (frontend bootstrap not wired — D8)", http.StatusBadRequest)
		return
	}

	claims, err := s.authDeps.verifier.Verify(accessToken)
	if err != nil {
		log.Printf("[auth] verify callback jwt: %v", err)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	userUUID, err := uuid.Parse(claims.Subject)
	if err != nil {
		http.Error(w, "invalid sub", http.StatusBadRequest)
		return
	}

	if err := upsertUserFromClaims(r.Context(), s.db, userUUID, claims); err != nil {
		log.Printf("[auth] upsert user %s: %v", userUUID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Trust the JWT's exp over the query-string expires_in — the
	// signed claim is authoritative; query params are not.
	jwtExp := claims.ExpiresAt
	// expires_in is logged-only — we don't use it as a source of truth.
	_ = expiresInStr

	sessExp := timeNow().Add(30 * 24 * time.Hour)
	sess, err := s.authDeps.sessions.Create(r.Context(), userUUID,
		accessToken, refreshToken, jwtExp, sessExp,
		r.UserAgent(), clientIP(r),
	)
	if err != nil {
		log.Printf("[auth] create session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID.String(),
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})

	http.Redirect(w, r, state.ReturnTo, http.StatusFound)
}

// handleLogout flips revoked_at on the current session and clears the
// cookie. Idempotent — repeated logouts don't 4xx.
//
// POST /api/auth/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authDeps == nil {
		http.NotFound(w, r)
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		if sid, perr := uuid.Parse(cookie.Value); perr == nil {
			if rerr := s.authDeps.sessions.Revoke(r.Context(), sid); rerr != nil {
				log.Printf("[auth] revoke %s: %v", sid, rerr)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		MaxAge: -1, HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated user's identity + org list.
// Wrapped in SessionMiddleware at mount time.
//
// GET /api/me  →  { id, email, display_name, avatar_url, github_username, orgs: [...] }
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims := ClaimsFrom(r.Context())
	if claims == nil {
		writeUnauth(w)
		return
	}
	type orgRow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	}
	type response struct {
		ID             string   `json:"id"`
		Email          string   `json:"email"`
		DisplayName    string   `json:"display_name,omitempty"`
		AvatarURL      string   `json:"avatar_url,omitempty"`
		GitHubUsername string   `json:"github_username,omitempty"`
		Orgs           []orgRow `json:"orgs"`
	}

	var resp response
	resp.Orgs = []orgRow{}

	err := s.db.QueryRowContext(r.Context(), `
		SELECT id::text, COALESCE(display_name, ''), COALESCE(avatar_url, ''), COALESCE(github_username, '')
		  FROM public.users
		 WHERE id = $1
	`, claims.Subject).Scan(&resp.ID, &resp.DisplayName, &resp.AvatarURL, &resp.GitHubUsername)
	if err != nil {
		log.Printf("[auth] /api/me user lookup %s: %v", claims.Subject, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp.Email = claims.Email

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT o.id::text, o.name, om.role
		  FROM org_memberships om
		  JOIN orgs o ON o.id = om.org_id
		 WHERE om.user_id = $1
		 ORDER BY o.name
	`, claims.Subject)
	if err != nil {
		log.Printf("[auth] /api/me org list %s: %v", claims.Subject, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var o orgRow
		if err := rows.Scan(&o.ID, &o.Name, &o.Role); err != nil {
			log.Printf("[auth] /api/me org scan: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp.Orgs = append(resp.Orgs, o)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// upsertUserFromClaims mirrors the user's identity from JWT claims
// into public.users. COALESCE preserves any field the claims happen
// to be missing — provider responses are inconsistent across users.
func upsertUserFromClaims(ctx context.Context, db *sql.DB, userID uuid.UUID, claims *verify.Claims) error {
	var displayName, avatarURL, ghUsername string
	if claims.UserMetadata != nil {
		displayName, _ = claims.UserMetadata["full_name"].(string)
		if displayName == "" {
			displayName, _ = claims.UserMetadata["name"].(string)
		}
		avatarURL, _ = claims.UserMetadata["avatar_url"].(string)
		ghUsername, _ = claims.UserMetadata["user_name"].(string)
		if ghUsername == "" {
			ghUsername, _ = claims.UserMetadata["preferred_username"].(string)
		}
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO public.users (id, display_name, avatar_url, github_username, created_at, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), now(), now())
		ON CONFLICT (id) DO UPDATE
		   SET display_name    = COALESCE(EXCLUDED.display_name,    public.users.display_name),
		       avatar_url      = COALESCE(EXCLUDED.avatar_url,      public.users.avatar_url),
		       github_username = COALESCE(EXCLUDED.github_username, public.users.github_username),
		       updated_at      = now()
	`, userID, displayName, avatarURL, ghUsername)
	return err
}

// gotrueRefreshFunc returns a closure that hits gotrue's /token endpoint
// with grant_type=refresh_token. Bound to the server so tests can swap
// it out without rebuilding the deps.
func (s *Server) gotrueRefreshFunc(cfg *authConfig) func(context.Context, string) (string, string, int64, error) {
	return func(ctx context.Context, refreshToken string) (string, string, int64, error) {
		body := strings.NewReader(url.Values{"refresh_token": {refreshToken}}.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			cfg.gotrueURL+"/token?grant_type=refresh_token", body)
		if err != nil {
			return "", "", 0, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", "", 0, fmt.Errorf("refresh http: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			return "", "", 0, fmt.Errorf("refresh http %d: %s", resp.StatusCode, bytes.TrimSpace(b))
		}
		var out struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", "", 0, fmt.Errorf("refresh decode: %w", err)
		}
		if out.AccessToken == "" || out.RefreshToken == "" {
			return "", "", 0, errors.New("refresh response missing tokens")
		}
		return out.AccessToken, out.RefreshToken,
			timeNow().Add(time.Duration(out.ExpiresIn) * time.Second).Unix(), nil
	}
}

// ---- state cookie ----------------------------------------------------------

const stateCookieName = "tf_oauth_state"

type stateClaims struct {
	ReturnTo  string `json:"return_to"`
	CSRF      string `json:"csrf"`
	ExpiresAt int64  `json:"exp"`
}

func (sc stateClaims) sign(key [32]byte) (string, error) {
	payload, err := json.Marshal(sc)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseStateCookie(raw string, key [32]byte) (*stateClaims, error) {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return nil, errors.New("malformed state cookie")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode mac: %w", err)
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return nil, errors.New("mac mismatch")
	}
	var sc stateClaims
	if err := json.Unmarshal(payload, &sc); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	if timeNow().Unix() > sc.ExpiresAt {
		return nil, errors.New("expired")
	}
	return &sc, nil
}

// normalizeReturnTo enforces relative-path-only and a default of "/".
// Anything starting with "//" (protocol-relative URL) or containing a
// scheme/host is rewritten to "/" — open-redirect protection.
func normalizeReturnTo(raw string) string {
	if raw == "" || raw == "/" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") {
		return "/"
	}
	if strings.HasPrefix(raw, "//") {
		return "/"
	}
	if u, err := url.Parse(raw); err == nil {
		if u.Scheme != "" || u.Host != "" {
			return "/"
		}
	}
	return raw
}

// isHTTPS detects whether the original request came in over TLS, even
// behind a reverse proxy that terminated TLS. Used to set the Secure
// flag on cookies — HTTPS-only in prod, but local-dev runs over HTTP
// and would otherwise refuse to accept the cookie at all.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		if i := strings.Index(xf, ","); i >= 0 {
			return strings.TrimSpace(xf[:i])
		}
		return strings.TrimSpace(xf)
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}
