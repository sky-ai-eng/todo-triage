package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
)

// ---------- JWKS test rig (mirrors internal/auth/verify/verifier_test.go) ----------

const (
	testIssuer   = "https://tf.test/auth/v1"
	testAudience = "authenticated"
)

type testKey struct {
	kid  string
	priv *rsa.PrivateKey
}

func newTestKey(t *testing.T) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	return testKey{kid: uuid.NewString(), priv: priv}
}

func (k testKey) publicJWK() map[string]any {
	pub := &k.priv.PublicKey
	return map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": k.kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (k testKey) mintJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = k.kid
	signed, err := tok.SignedString(k.priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

type jwksMux struct {
	mu   sync.Mutex
	keys []testKey
}

func (m *jwksMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	jwks := make([]map[string]any, 0, len(m.keys))
	for _, k := range m.keys {
		jwks = append(jwks, k.publicJWK())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
}

// validClaimsFor returns a JWT claim map for the given user id.
func validClaimsFor(userID uuid.UUID) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   userID.String(),
		"email": userID.String() + "@test",
		"iss":   testIssuer,
		"aud":   testAudience,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "github",
			"providers": []any{"github"},
		},
		"user_metadata": map[string]any{
			"user_name":  "test-user-" + userID.String()[:8],
			"avatar_url": "https://avatar.example/" + userID.String()[:8],
			"full_name":  "Test User",
		},
	}
}

// ---------- test rig: server + JWKS + cleanup ----------

type authRig struct {
	t           *testing.T
	h           *pgtest.Harness
	srv         *Server
	jwks        *httptest.Server
	jwksMux     *jwksMux
	signKey     testKey
	masterKey   [32]byte
	gotrueStub  *httptest.Server // optional; for refresh tests
	refreshedTo string           // last new access token issued by the stub
}

// newAuthRig boots the pg testcontainer, JWKS test server, verifier,
// session store, and Server. The test process owns the JWKS keypair so
// it can mint tokens that the live Verifier accepts.
func newAuthRig(t *testing.T) *authRig {
	t.Helper()

	h := pgtest.Shared(t)
	h.Reset(t)

	signKey := newTestKey(t)
	mux := &jwksMux{keys: []testKey{signKey}}
	jwks := httptest.NewServer(mux)
	t.Cleanup(jwks.Close)

	v, err := verify.NewVerifier(context.Background(), jwks.URL, testIssuer, testAudience)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	var masterKey [32]byte
	if _, err := rand.Read(masterKey[:]); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	store := sessions.NewStore(h.AdminDB, sessions.Key(masterKey))

	// Construct Server with nil store dependencies — auth tests don't
	// touch the prompt/swipe/etc. handlers. server.New panics if it
	// tries to do anything with them at construction time, but a
	// quick scan of New shows it only stashes the pointers, doesn't
	// call methods.
	s := New(h.AdminDB, nil, nil, nil, nil, nil, nil, nil, nil)

	if err := s.SetAuthDeps(v, store, "http://gotrue.unused", "http://tf.test", masterKey); err != nil {
		t.Fatalf("SetAuthDeps: %v", err)
	}

	return &authRig{
		t:         t,
		h:         h,
		srv:       s,
		jwks:      jwks,
		jwksMux:   mux,
		signKey:   signKey,
		masterKey: masterKey,
	}
}

// seedUser inserts auth.users + public.users for a fresh UUID and
// returns it. Mirrors pgtest.seedUser (package-private) — when a third
// caller materializes, this lifts to pgtest as an exported helper.
func (r *authRig) seedUser() uuid.UUID {
	r.t.Helper()
	var idStr string
	if err := r.h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		r.t.Fatalf("gen uuid: %v", err)
	}
	r.h.SeedAuthUser(r.t, idStr, idStr+"@test")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO users (id, display_name) VALUES ($1, $2)`, idStr, "tester"); err != nil {
		r.t.Fatalf("seed public.users: %v", err)
	}
	return uuid.MustParse(idStr)
}

// seedOrg inserts an org owned by ownerID and returns its UUID + the
// id of its default team. Org_memberships gets a 'owner' row for the
// owner; team memberships get 'admin'.
func (r *authRig) seedOrg(ownerID uuid.UUID, slug string) (orgID, teamID uuid.UUID) {
	r.t.Helper()
	var oID, tID string
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO orgs (slug, name, owner_user_id) VALUES ($1, $1, $2) RETURNING id::text
	`, slug, ownerID).Scan(&oID); err != nil {
		r.t.Fatalf("insert org: %v", err)
	}
	if err := r.h.AdminDB.QueryRow(`
		INSERT INTO teams (org_id, slug, name) VALUES ($1, 'default', 'default') RETURNING id::text
	`, oID).Scan(&tID); err != nil {
		r.t.Fatalf("insert team: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'owner')`,
		ownerID, oID); err != nil {
		r.t.Fatalf("insert org_membership: %v", err)
	}
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'admin')`,
		ownerID, tID); err != nil {
		r.t.Fatalf("insert membership: %v", err)
	}
	return uuid.MustParse(oID), uuid.MustParse(tID)
}

// signStateCookie returns a state cookie value usable in callback
// requests. Mirrors handleOAuthStart's signing path without forcing
// the test to drive the full redirect.
func (r *authRig) signStateCookie(returnTo, csrf string) string {
	r.t.Helper()
	state := stateClaims{
		ReturnTo: returnTo, CSRF: csrf,
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	cfg := r.srv.authCfg
	signed, err := state.sign(cfg.stateKey)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}
	return signed
}

// driveCallback fires the callback handler with the given access /
// refresh tokens and returns the resulting response (including the
// Set-Cookie sid value).
func (r *authRig) driveCallback(accessToken, refreshToken string) *http.Response {
	r.t.Helper()
	csrf := "test-csrf-" + uuid.NewString()
	stateVal := r.signStateCookie("/dashboard", csrf)

	q := url.Values{}
	q.Set("state", csrf)
	q.Set("access_token", accessToken)
	q.Set("refresh_token", refreshToken)
	q.Set("expires_in", "3600")
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateVal})

	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// sidFromResp pulls the sid cookie from a callback response, fatal on miss.
func sidFromResp(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	t.Fatalf("response had no %s cookie (status=%d, set-cookie=%v)",
		sessionCookieName, resp.StatusCode, resp.Header["Set-Cookie"])
	return ""
}

// requestWithSid fires a request to s.mux with the sid cookie set.
func (r *authRig) requestWithSid(method, path, sid string) *http.Response {
	r.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	}
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

// ---------- tests: SKY-251 acceptance bullets ----------

// Bullet 1: Login → /api/me returns user + org list.
func TestAuthFlow_LoginToMe(t *testing.T) {
	r := newAuthRig(t)

	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "alice-org")

	token := r.signKey.mintJWT(t, validClaimsFor(userID))

	// Drive callback: state cookie set, tokens supplied, expect 302 + sid cookie.
	resp := r.driveCallback(token, "fake-refresh-token")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := sidFromResp(t, resp)

	// GET /api/me with the sid cookie.
	meResp := r.requestWithSid("GET", "/api/me", sid)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/api/me status=%d, want 200", meResp.StatusCode)
	}
	var me struct {
		ID             string `json:"id"`
		Email          string `json:"email"`
		GitHubUsername string `json:"github_username"`
		Orgs           []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"orgs"`
	}
	if err := json.NewDecoder(meResp.Body).Decode(&me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.ID != userID.String() {
		t.Errorf("me.id = %q, want %q", me.ID, userID)
	}
	if me.Email != userID.String()+"@test" {
		t.Errorf("me.email = %q", me.Email)
	}
	if want := "test-user-" + userID.String()[:8]; me.GitHubUsername != want {
		t.Errorf("me.github_username = %q, want %q", me.GitHubUsername, want)
	}
	if len(me.Orgs) != 1 || me.Orgs[0].ID != orgID.String() {
		t.Errorf("me.orgs = %v, want one org %s", me.Orgs, orgID)
	}
	if me.Orgs[0].Role != "owner" {
		t.Errorf("me.orgs[0].role = %q, want owner", me.Orgs[0].Role)
	}
}

// Bullet 2: Logout flips revoked_at; subsequent requests 401; row persists.
func TestAuthFlow_LogoutFlipsRevoked(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	token := r.signKey.mintJWT(t, validClaimsFor(userID))
	resp := r.driveCallback(token, "fake-refresh")
	sid := sidFromResp(t, resp)

	// Sanity: /api/me works before logout.
	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusOK {
		t.Fatalf("pre-logout /api/me = %d, want 200", got)
	}

	// Logout.
	logoutResp := r.requestWithSid("POST", "/api/auth/logout", sid)
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d, want 204", logoutResp.StatusCode)
	}

	// /api/me now 401.
	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("post-logout /api/me = %d, want 401", got)
	}

	// Row persists with revoked_at NOT NULL.
	sidUUID := uuid.MustParse(sid)
	var revokedAt *time.Time
	if err := r.h.AdminDB.QueryRow(
		`SELECT revoked_at FROM public.sessions WHERE id = $1`, sidUUID,
	).Scan(&revokedAt); err != nil {
		t.Fatalf("post-logout select: %v", err)
	}
	if revokedAt == nil {
		t.Fatal("logout did not set revoked_at")
	}
}

// Bullet 3: ciphertext at rest. Covered in sessions/store_test.go
// (TestStore_CiphertextAtRest) — re-asserting in this test file would
// duplicate without adding signal.

// Bullet 4: force-expiry. Setting expires_at in the past forces re-login
// even if the JWT is still individually valid.
func TestAuthFlow_ForceExpiryReturns401(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")

	token := r.signKey.mintJWT(t, validClaimsFor(userID))
	resp := r.driveCallback(token, "fake-refresh")
	sid := sidFromResp(t, resp)

	// Backdate expires_at past now. Must also push created_at into the
	// past to satisfy CHECK (expires_at > created_at).
	sidUUID := uuid.MustParse(sid)
	if _, err := r.h.AdminDB.Exec(`
		UPDATE public.sessions
		   SET created_at     = now() - interval '2 hours',
		       jwt_expires_at = now() - interval '1 hour 30 minutes',
		       expires_at     = now() - interval '1 minute'
		 WHERE id = $1`, sidUUID); err != nil {
		t.Fatalf("force expiry: %v", err)
	}

	if got := r.requestWithSid("GET", "/api/me", sid).StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("expired /api/me = %d, want 401", got)
	}
}

// Bullet 5: cross-org leakage. User A in Org A; hits /api/orgs/{B}/probe
// → 404 (not 403 — don't leak Org B's existence).
//
// Bullet 6: within-org role. User as 'viewer' in Org A; verb-restricted
// op returns 403. (Within-org existence is already disclosed by
// membership.)
//
// Both bullets require a route registered with OrgMiddleware (and, for
// bullet 6, a role-aware handler). D9 does the bulk retrofit; here we
// mount a probe handler directly to exercise the middleware-side
// behavior that D7 is responsible for.
func TestAuthFlow_OrgMiddleware_CrossOrg404AndMember200(t *testing.T) {
	r := newAuthRig(t)

	// Register a probe route on the live mux. Wrapped in withSession
	// + withOrg matches what production org-scoped routes will look
	// like after D9.
	r.srv.mux.Handle("GET /api/orgs/{org_id}/probe",
		r.srv.withSession(r.srv.withOrg(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))))

	userA := r.seedUser()
	orgA, _ := r.seedOrg(userA, "alice-org")

	userB := r.seedUser()
	orgB, _ := r.seedOrg(userB, "bob-org")

	// Sign User A in.
	token := r.signKey.mintJWT(t, validClaimsFor(userA))
	resp := r.driveCallback(token, "fake-refresh")
	sid := sidFromResp(t, resp)

	// User A → /api/orgs/{orgA}/probe = 200
	gotA := r.requestWithSid("GET", "/api/orgs/"+orgA.String()+"/probe", sid).StatusCode
	if gotA != http.StatusOK {
		t.Errorf("User A → orgA: got %d, want 200", gotA)
	}

	// User A → /api/orgs/{orgB}/probe = 404
	gotB := r.requestWithSid("GET", "/api/orgs/"+orgB.String()+"/probe", sid).StatusCode
	if gotB != http.StatusNotFound {
		t.Errorf("User A → orgB: got %d, want 404 (not 403 — don't leak existence)", gotB)
	}

	// Also verify the path-malformed case 404s (not 400).
	gotBad := r.requestWithSid("GET", "/api/orgs/not-a-uuid/probe", sid).StatusCode
	if gotBad != http.StatusNotFound {
		t.Errorf("malformed org_id: got %d, want 404", gotBad)
	}

	_ = userB
}

func TestAuthFlow_NoSidCookie_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.requestWithSid("GET", "/api/me", "").StatusCode
	if got != http.StatusUnauthorized {
		t.Errorf("no cookie /api/me = %d, want 401", got)
	}
}

func TestAuthFlow_GarbageSidCookie_Returns401(t *testing.T) {
	r := newAuthRig(t)
	got := r.requestWithSid("GET", "/api/me", "not-a-uuid").StatusCode
	if got != http.StatusUnauthorized {
		t.Errorf("garbage cookie /api/me = %d, want 401", got)
	}
}

func TestAuthFlow_Callback_RejectsBadState(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	r.seedOrg(userID, "alice-org")
	token := r.signKey.mintJWT(t, validClaimsFor(userID))

	// Wrong CSRF in query vs cookie.
	stateCookie := r.signStateCookie("/dashboard", "the-real-csrf")
	q := url.Values{}
	q.Set("state", "different-csrf")
	q.Set("access_token", token)
	q.Set("refresh_token", "ref")
	q.Set("expires_in", "3600")
	req := httptest.NewRequest("GET", "/api/auth/callback?"+q.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: stateCookie})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("state mismatch status = %d, want 400", rec.Code)
	}
}

// ---------- standalone tests: state cookie + return_to normalization ----------

func TestStateCookie_Roundtrip(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sc := stateClaims{ReturnTo: "/dashboard", CSRF: "abc", ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}
	signed, err := sc.sign(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := parseStateCookie(signed, key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ReturnTo != "/dashboard" || got.CSRF != "abc" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestStateCookie_WrongKey(t *testing.T) {
	var k1, k2 [32]byte
	_, _ = rand.Read(k1[:])
	_, _ = rand.Read(k2[:])
	sc := stateClaims{ReturnTo: "/", CSRF: "x", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	signed, _ := sc.sign(k1)
	if _, err := parseStateCookie(signed, k2); err == nil {
		t.Fatal("parse with wrong key succeeded")
	}
}

func TestStateCookie_Expired(t *testing.T) {
	var key [32]byte
	_, _ = rand.Read(key[:])
	sc := stateClaims{ReturnTo: "/", CSRF: "x", ExpiresAt: time.Now().Add(-1 * time.Minute).Unix()}
	signed, _ := sc.sign(key)
	if _, err := parseStateCookie(signed, key); err == nil {
		t.Fatal("parse of expired cookie succeeded")
	}
}

func TestNormalizeReturnTo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/dashboard", "/dashboard"},
		{"/orgs/123/tasks", "/orgs/123/tasks"},
		{"//evil.com/path", "/"},     // protocol-relative
		{"https://evil.com/", "/"},   // absolute URL
		{"javascript:alert(1)", "/"}, // scheme-relative
		{"path-without-slash", "/"},
	}
	for _, tc := range cases {
		if got := normalizeReturnTo(tc.in); got != tc.want {
			t.Errorf("normalizeReturnTo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
