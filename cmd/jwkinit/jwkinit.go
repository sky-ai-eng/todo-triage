// Package jwkinit is the CLI entrypoint for the `triagefactory jwk-init`
// subcommand. It generates the RS256 keypair that GoTrue uses to sign
// access tokens.
//
// GoTrue's GOTRUE_JWT_KEYS expects a JSON ARRAY of JWK objects (each
// carrying both public and private material), NOT the RFC-7517-wrapped
// {"keys": [...]} JWKS form. The Verifier consumes the standard JWKS
// from GoTrue's /.well-known/jwks.json endpoint; GoTrue translates
// between the two shapes internally.
//
// Default emits to stdout. With --write-env <path>, appends a single
// GOTRUE_JWT_KEYS=<json> line to the given .env file so the operator can
// pipe install setup into one step.
//
// Also exposes --verify, which reads one JWT from stdin and verifies it
// against the JWKS at TF_GOTRUE_JWKS_URL — the operator-facing version of
// the unit-test rotation smoke check.
package jwkinit

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
)

// Handle dispatches from main.go on `triagefactory jwk-init ...`.
func Handle(args []string) {
	fs := flag.NewFlagSet("jwk-init", flag.ExitOnError)
	writeEnv := fs.String("write-env", "", "append GOTRUE_JWT_KEYS=<jwks> to this .env file instead of printing to stdout")
	verifyMode := fs.Bool("verify", false, "read a JWT from stdin and verify it against TF_GOTRUE_JWKS_URL / TF_GOTRUE_ISSUER")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *verifyMode {
		if err := runVerify(); err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := runGenerate(*writeEnv); err != nil {
		fmt.Fprintf(os.Stderr, "jwk-init: %v\n", err)
		os.Exit(1)
	}
}

func runGenerate(writeEnvPath string) error {
	keys, err := generateGoTrueKeys()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return fmt.Errorf("marshal keys: %w", err)
	}

	// GoTrue's config validation still requires GOTRUE_JWT_SECRET to be non-empty
	// even when ALGORITHM=RS256 (it's a legacy HS256 fallback that the asymmetric
	// path doesn't actually exercise). Generate a random value alongside the
	// JWKS so the operator install flow stays one command — and so the unused
	// secret is at least cryptographically random rather than a sentinel string.
	jwtSecret, err := randomHexSecret(32)
	if err != nil {
		return fmt.Errorf("generate jwt secret: %w", err)
	}

	if writeEnvPath == "" {
		// Pretty-print stdout for human inspection; the .env form is compact.
		pretty, _ := json.MarshalIndent(keys, "", "  ")
		fmt.Println(string(pretty))
		fmt.Println()
		fmt.Println("# also set GOTRUE_JWT_SECRET (required by gotrue config; unused under RS256):")
		fmt.Printf("GOTRUE_JWT_SECRET=%s\n", jwtSecret)
		return nil
	}

	// O_RDWR (not O_WRONLY) so we can ReadAt the last byte before appending —
	// otherwise an existing .env that doesn't end in \n could produce a
	// `TF_SESSION_KEY=...GOTRUE_JWT_KEYS=...` smush on one line.
	f, err := os.OpenFile(writeEnvPath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()
	// OpenFile's mode arg only applies on CREATE; explicit chmod handles the
	// common case where .env already exists with looser perms.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod env file: %w", err)
	}
	if err := ensureTrailingNewline(f); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "GOTRUE_JWT_KEYS=%s\nGOTRUE_JWT_SECRET=%s\n", string(encoded), jwtSecret); err != nil {
		return fmt.Errorf("write env lines: %w", err)
	}
	fmt.Fprintf(os.Stderr, "appended GOTRUE_JWT_KEYS + GOTRUE_JWT_SECRET to %s\n", writeEnvPath)
	return nil
}

func runVerify() error {
	jwksURL := os.Getenv("TF_GOTRUE_JWKS_URL")
	issuer := os.Getenv("TF_GOTRUE_ISSUER")
	audience := os.Getenv("TF_GOTRUE_AUD")
	if audience == "" {
		audience = "authenticated"
	}

	v, err := verify.NewVerifier(context.Background(), jwksURL, issuer, audience)
	if err != nil {
		return fmt.Errorf("init verifier: %w", err)
	}

	tokenBytes, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return fmt.Errorf("stdin: empty")
	}

	claims, err := v.Verify(token)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(claims, "", "  ")
	fmt.Println(string(out))
	return nil
}

// generateGoTrueKeys produces GoTrue's GOTRUE_JWT_KEYS shape — a JSON
// array of JWK objects, each with both public and private RSA material.
// GoTrue exposes only the public side at /.well-known/jwks.json.
func generateGoTrueKeys() ([]map[string]any, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("rsa generate: %w", err)
	}
	priv.Precompute()

	kid := uuid.NewString()
	jwk := map[string]any{
		"kty": "RSA",
		"use": "sig",
		// GoTrue's JwtKeysDecoder.Validate requires exactly one private key
		// in the set with key_ops containing "sign". Setting it here marks
		// this entry as the active signing key; the derived public-side JWK
		// gets key_ops=["verify"] applied automatically by GoTrue.
		"key_ops": []string{"sign"},
		"alg":     "RS256",
		"kid":     kid,
		"n":       b64(priv.N.Bytes()),
		"e":       b64(big.NewInt(int64(priv.E)).Bytes()),
		"d":       b64(priv.D.Bytes()),
		"p":       b64(priv.Primes[0].Bytes()),
		"q":       b64(priv.Primes[1].Bytes()),
		"dp":      b64(priv.Precomputed.Dp.Bytes()),
		"dq":      b64(priv.Precomputed.Dq.Bytes()),
		"qi":      b64(priv.Precomputed.Qinv.Bytes()),
	}
	return []map[string]any{jwk}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// randomHexSecret returns 2*nBytes hex chars from a crypto/rand source.
// Hex avoids any shell-quoting concerns when the value gets exported into
// a docker-compose env block.
func randomHexSecret(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureTrailingNewline checks the last byte of an O_APPEND-opened file and
// writes a \n if it's missing. Required because an existing .env that the
// operator hand-edited without a trailing newline would otherwise concatenate
// our new line onto the previous one.
func ensureTrailingNewline(f *os.File) error {
	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat env file: %w", err)
	}
	if stat.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, stat.Size()-1); err != nil {
		return fmt.Errorf("read last byte of env file: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("write leading newline: %w", err)
	}
	return nil
}

const usage = `triagefactory jwk-init — generate the GoTrue RS256 signing key.

USAGE
  triagefactory jwk-init                       print JWKS + a fresh random
                                               GOTRUE_JWT_SECRET to stdout
  triagefactory jwk-init --write-env .env      append GOTRUE_JWT_KEYS=<jwks>
                                               AND GOTRUE_JWT_SECRET=<hex>
                                               to .env (both are required by
                                               GoTrue's config validation)
  triagefactory jwk-init --verify              read JWT from stdin; verify
                                               against TF_GOTRUE_JWKS_URL +
                                               TF_GOTRUE_ISSUER; print claims

NOTES
  The JWKS contains private material — treat the output like a secret. Only
  the public side is published by GoTrue at /.well-known/jwks.json. The
  GOTRUE_JWT_SECRET is the legacy HS256 fallback that GoTrue config
  validation still requires even under RS256; the value isn't used for
  signing but is required to be non-empty.

  Re-running rotates the key; recreate the GoTrue container
  (docker compose up -d gotrue — NOT docker compose start) to pick up the
  new env.
`
