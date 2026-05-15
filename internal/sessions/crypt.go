// Package sessions owns multi-mode user sessions: the AES-GCM envelope
// that encrypts GoTrue access + refresh tokens at rest, and the
// public.sessions CRUD that wraps them.
//
// Local mode never imports this package — sessions only exist in
// multi-mode (cookie-bearer auth backed by GoTrue). See
// docs/multi-tenant-architecture.html §4 + §13 D7.
package sessions

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
)

// Key is a 32-byte fixed-size key. Used for both the AES-256 session
// encryption key and the HMAC-SHA256 cookie-signing secret — same
// loader/decoder code, semantically distinct callers.
type Key [32]byte

// Operator-facing env var names. Kept as exported constants so the
// runtime loader, docs, and any future helper tooling can't drift on
// the spelling.
//
// The two are kept separate (rather than one master key with HKDF
// subkeys) because:
//   - Rotating the cookie secret should not invalidate every active
//     session's encrypted blobs, and vice versa
//   - It matches the convention in skynet/authkit
//     (SESSION_ENCRYPTION_KEY + a separate signing pepper)
const (
	EnvSessionEncryptionKey = "TF_SESSION_ENCRYPTION_KEY" // AES-GCM at-rest key for jwt_enc / refresh_token_enc
	EnvCookieSecret         = "TF_COOKIE_SECRET"          // HMAC key for the short-lived OAuth state cookie
)

// LoadKeyFromEnv reads the named env var and decodes it as either hex
// (64 chars) or standard base64. Returns a clear error rather than
// panicking — main.go's multi-mode branch fails fast on this,
// surfacing a readable startup error.
//
// Both EnvSessionEncryptionKey and EnvCookieSecret use this same
// loader; the caller names the variable.
func LoadKeyFromEnv(envVar string) (Key, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return Key{}, fmt.Errorf("%s is empty (generate with `openssl rand -hex 32`)", envVar)
	}
	b, err := decodeKey(raw)
	if err != nil {
		return Key{}, fmt.Errorf("%s: %w", envVar, err)
	}
	if len(b) != 32 {
		return Key{}, fmt.Errorf("%s must decode to 32 bytes, got %d", envVar, len(b))
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// decodeKey accepts hex first (the documented format), falling back to
// standard base64. We don't try URL-safe base64 because `openssl rand
// -base64 32` (the alternative we mention in docs) emits standard.
func decodeKey(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, errors.New("not a valid hex or base64 string")
}

// Encrypt returns (ciphertext, nonce) for a fresh AES-GCM encryption.
// Nonce is 12 bytes from crypto/rand — never reuse across (key, plaintext)
// pairs. We store the nonce as a separate column in public.sessions
// rather than prefixing the ciphertext so the schema is self-describing.
func (k Key) Encrypt(plaintext []byte) (ciphertext, nonce []byte, err error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("gcm: %w", err)
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("nonce: %w", err)
	}
	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return ciphertext, nonce, nil
}

// LogID returns a short, non-reversible identifier for a session UUID
// suitable for log lines and error messages. Implementation: first 8
// hex chars of SHA-256(sid). Properties:
//   - One-way: an attacker reading logs can't recover the sid
//   - Stable: the same sid always maps to the same prefix, so a
//     support engineer can correlate "this user's session keeps
//     failing refresh" by spotting repeats
//   - Collision-resistant at our scale: 8 hex chars = 32 bits ≈ 4B
//     possibilities; for 10K active sessions, the birthday-collision
//     probability is negligible
//
// Production log lines should always wrap sid arguments through this
// helper. Logging the raw UUID gives an attacker who exfiltrates logs
// a roster of recently-valid session ids that could be paired with
// stolen cookies for replay.
func LogID(sid uuid.UUID) string {
	sum := sha256.Sum256(sid[:])
	return hex.EncodeToString(sum[:4])
}

// Decrypt inverts Encrypt. Returns an error on either auth-tag failure
// (tampered ciphertext) or wrong-key — AEAD doesn't distinguish those,
// which is the point. Callers render "session invalid" rather than
// surfacing the underlying error to the user.
func (k Key) Decrypt(ciphertext, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("nonce length %d, want %d", len(nonce), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}
