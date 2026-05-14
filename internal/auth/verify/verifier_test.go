package verify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://gotrue.test/auth/v1"
	testAudience = "authenticated"
)

// testKey holds one signing key plus its public JWK projection. mintJWT signs
// with the private side; the JWKS server below serves only the public side.
type testKey struct {
	kid  string
	priv *rsa.PrivateKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa generate: %v", err)
	}
	return testKey{kid: kid, priv: priv}
}

func (k testKey) publicJWK() map[string]any {
	pub := &k.priv.PublicKey
	return map[string]any{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": k.kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
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

// jwksMux holds the current set of public keys served at /.well-known/jwks.json.
// It supports swap() so the rotation test can change keys mid-flight.
type jwksMux struct {
	mu   sync.Mutex
	keys []testKey
	hits atomic.Int64
}

func newJWKSMux(keys ...testKey) *jwksMux {
	return &jwksMux{keys: keys}
}

func (m *jwksMux) swap(keys ...testKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = keys
}

func (m *jwksMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.hits.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	jwks := make([]map[string]any, 0, len(m.keys))
	for _, k := range m.keys {
		jwks = append(jwks, k.publicJWK())
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": jwks})
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   "00000000-0000-0000-0000-000000000001",
		"email": "test@example.com",
		"iss":   testIssuer,
		"aud":   testAudience,
		"role":  "authenticated",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "github",
			"providers": []any{"github"},
		},
		"user_metadata": map[string]any{
			"preferred_username": "alice",
			"avatar_url":         "https://avatars.example/alice",
			"full_name":          "Alice Example",
		},
	}
}

func newVerifier(t *testing.T, jwksURL string) *Verifier {
	t.Helper()
	v, err := NewVerifier(context.Background(), jwksURL, testIssuer, testAudience)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestVerify_HappyPath(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	token := key.mintJWT(t, validClaims())

	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Subject != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("sub: got %q", claims.Subject)
	}
	if claims.Email != "test@example.com" {
		t.Errorf("email: got %q", claims.Email)
	}
	if claims.Provider != "github" {
		t.Errorf("provider: got %q", claims.Provider)
	}
	if got, _ := claims.UserMetadata["preferred_username"].(string); got != "alice" {
		t.Errorf("user_metadata.preferred_username: got %q", got)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	token := key.mintJWT(t, validClaims())

	// flip a byte in the signature segment
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sig[0] ^= 0xFF
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	tampered := strings.Join(parts, ".")

	if _, err := v.Verify(tampered); err == nil {
		t.Fatal("Verify(tampered): expected error, got nil")
	}
}

func TestVerify_WrongIssuer(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	c := validClaims()
	c["iss"] = "https://imposter.test"
	token := key.mintJWT(t, c)

	if _, err := v.Verify(token); err == nil {
		t.Fatal("Verify(wrong-iss): expected error, got nil")
	}
}

func TestVerify_WrongAudience(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	c := validClaims()
	c["aud"] = "service-role"
	token := key.mintJWT(t, c)

	if _, err := v.Verify(token); err == nil {
		t.Fatal("Verify(wrong-aud): expected error, got nil")
	}
}

func TestVerify_Expired(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	c := validClaims()
	c["exp"] = time.Now().Add(-time.Hour).Unix()
	token := key.mintJWT(t, c)

	if _, err := v.Verify(token); err == nil {
		t.Fatal("Verify(expired): expected error, got nil")
	}
}

func TestVerify_MissingExp(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)
	c := validClaims()
	delete(c, "exp")
	token := key.mintJWT(t, c)

	if _, err := v.Verify(token); err == nil {
		t.Fatal("Verify(no-exp): expected error, got nil")
	}
}

func TestVerify_HS256DowngradeRejected(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)

	// Hand-craft an HS256-signed token using the public modulus bytes
	// as the "shared secret" — a classic confused-deputy downgrade attack.
	c := validClaims()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = key.kid
	secret := key.priv.N.Bytes()
	signed, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign hs256: %v", err)
	}

	if _, err := v.Verify(signed); err == nil {
		t.Fatal("Verify(hs256-downgrade): expected error, got nil")
	}
}

func TestVerify_AlgNoneRejected(t *testing.T) {
	key := newTestKey(t, "key-1")
	srv := httptest.NewServer(newJWKSMux(key))
	defer srv.Close()

	v := newVerifier(t, srv.URL)

	c := validClaims()
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, c)
	tok.Header["kid"] = key.kid
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}

	if _, err := v.Verify(signed); err == nil {
		t.Fatal("Verify(alg=none): expected error, got nil")
	}
}

func TestVerify_UnknownKidRefresh(t *testing.T) {
	keyA := newTestKey(t, "key-A")
	mux := newJWKSMux(keyA)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	v := newVerifier(t, srv.URL)

	// Sanity: key-A works.
	if _, err := v.Verify(keyA.mintJWT(t, validClaims())); err != nil {
		t.Fatalf("Verify(keyA): %v", err)
	}

	// Rotate: server now serves only key-B. A token signed by key-B has a
	// kid the verifier hasn't seen — keyfunc/v3 must refresh and find it.
	keyB := newTestKey(t, "key-B")
	mux.swap(keyB)

	token := keyB.mintJWT(t, validClaims())
	if _, err := v.Verify(token); err != nil {
		t.Fatalf("Verify(keyB after rotation): %v", err)
	}

	// And key-A tokens no longer verify (kid not present).
	if _, err := v.Verify(keyA.mintJWT(t, validClaims())); err == nil {
		t.Fatal("Verify(keyA after rotation): expected error, got nil")
	}
}

func TestNewVerifier_RejectsEmptyConfig(t *testing.T) {
	for name, args := range map[string][3]string{
		"empty-url":      {"", testIssuer, testAudience},
		"empty-issuer":   {"http://x", "", testAudience},
		"empty-audience": {"http://x", testIssuer, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVerifier(context.Background(), args[0], args[1], args[2]); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
