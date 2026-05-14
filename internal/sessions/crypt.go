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
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// Key is the AES-256 master key loaded once at startup from TF_SESSION_KEY.
// The arch doc (§4 line 1048) calls for a 32-byte key used directly — no
// KDF — so this type is a fixed-size array rather than a slice.
type Key [32]byte

// EnvVar is the env var name we read the key from. Exported as a constant
// so the operator-facing tooling (jwk-init, docs) and the runtime loader
// can never drift.
const EnvVar = "TF_SESSION_KEY"

// LoadKeyFromEnv reads TF_SESSION_KEY and decodes it as either hex (64
// chars) or standard base64. The two-format acceptance matches our
// generation guidance — `openssl rand -hex 32` is the recommended path
// (URL-safe, paste-safe), but a base64 value from another tool should
// also work without forcing a re-encode step.
//
// Returns a clear error rather than panicking — main.go's multi-mode
// branch fails fast on this, surfacing a readable startup error.
func LoadKeyFromEnv() (Key, error) {
	raw := os.Getenv(EnvVar)
	if raw == "" {
		return Key{}, fmt.Errorf("%s is empty (generate with `openssl rand -hex 32`)", EnvVar)
	}
	b, err := decodeKey(raw)
	if err != nil {
		return Key{}, fmt.Errorf("%s: %w", EnvVar, err)
	}
	if len(b) != 32 {
		return Key{}, fmt.Errorf("%s must decode to 32 bytes, got %d", EnvVar, len(b))
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
