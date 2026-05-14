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

	if writeEnvPath == "" {
		// Pretty-print stdout for human inspection; the .env form is compact.
		pretty, _ := json.MarshalIndent(keys, "", "  ")
		fmt.Println(string(pretty))
		return nil
	}

	f, err := os.OpenFile(writeEnvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod env file: %w", err)
	}
	if _, err := fmt.Fprintf(f, "GOTRUE_JWT_KEYS=%s\n", string(encoded)); err != nil {
		return fmt.Errorf("write env line: %w", err)
	}
	fmt.Fprintf(os.Stderr, "appended GOTRUE_JWT_KEYS to %s\n", writeEnvPath)
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

const usage = `triagefactory jwk-init — generate the GoTrue RS256 signing key.

USAGE
  triagefactory jwk-init                       print JWKS to stdout (pretty)
  triagefactory jwk-init --write-env .env      append GOTRUE_JWT_KEYS=<jwks> to .env
  triagefactory jwk-init --verify              read JWT from stdin; verify
                                               against TF_GOTRUE_JWKS_URL +
                                               TF_GOTRUE_ISSUER; print claims

NOTES
  The JWKS contains private material — treat the output like a secret. Only
  the public side is published by GoTrue at /.well-known/jwks.json.
  Re-running rotates the key; restart GoTrue to pick up the new value.
`
