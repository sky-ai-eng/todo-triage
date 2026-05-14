# Self-host setup (multi-mode)

This is the operator-facing install flow for the multi-tenant deployment. **Local mode (default, `TF_MODE=local`) needs none of this** — install Triage Factory normally, no Postgres or GoTrue required.

## 1. Create a GitHub OAuth app

Go to https://github.com/settings/developers → New OAuth App.

- **Homepage URL:** your public TF URL (e.g. `https://triagefactory.yourcompany.com`)
- **Authorization callback URL:** `${TF_PUBLIC_URL}/auth/v1/callback`

This is GoTrue's callback, not the TF callback handler — GitHub redirects here after the user authorizes, GoTrue exchanges the code, and then GoTrue 302s the browser back to the TF callback path (set per-request via the `redirect_to` query param on `/authorize`).

Save the **Client ID** and **Client secret**.

## 2. Populate `.env`

```sh
cp .env.example .env
```

Fill in:
- `POSTGRES_PASSWORD` — superuser password. Used for migrations and admin tasks. Generate with `openssl rand -base64 32`.
- `SUPABASE_AUTH_ADMIN_PASSWORD` — distinct password for the role GoTrue connects as. Keeping it separate from the superuser means a GoTrue compromise doesn't surrender full DB access. Generate with a URL-safe character set, for example `openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c 32; echo`, because this value is interpolated directly into `GOTRUE_DB_DATABASE_URL`. If you use a password containing reserved URL characters such as `/`, percent-encode it before Compose uses it in the URL.
- `TF_PUBLIC_URL` — your public URL (no trailing slash)
- `GH_CLIENT_ID` / `GH_CLIENT_SECRET` — from step 1

Leave `TF_SESSION_KEY` empty for now (D7 wires it).

> **Rotating passwords:** edit `.env` and re-run `docker compose up -d`. A short-lived `postgres-postinit` sidecar runs on every boot and reapplies `ALTER USER` for the non-superuser roles, so password changes propagate without wiping the data volume. Rotating `POSTGRES_PASSWORD` itself requires more care — that's the superuser's password and Postgres only honors the env var on first init, so changing it means `ALTER USER postgres WITH PASSWORD '...'` by hand inside the running container.

## 3. Generate the JWT signing key

```sh
./triagefactory jwk-init --write-env .env
```

This generates a fresh RS256 keypair, formats it as a JWKS containing both private and public material, and appends `GOTRUE_JWT_KEYS=<json>` to `.env`. The private side stays in `.env` (read only by GoTrue); only the public side is published at GoTrue's `/.well-known/jwks.json` endpoint.

Re-running `jwk-init --write-env .env` appends a *second* line, which works (GoTrue picks the last one) but is messy — clear the existing line first if you're rotating.

## 4. Bring up the stack

```sh
docker compose up -d
```

This starts Postgres + GoTrue. The Postgres image is `supabase/postgres`, which pre-provisions the `auth` schema, the `supabase_auth_admin` role GoTrue connects as, and the vault / pgsodium / pgvector extensions D5+ will use.

The Triage Factory binary itself runs from the host (D13 will package it as a container image; D9 will wire its own DB connection):

```sh
TF_MODE=multi \
  TF_GOTRUE_URL=http://localhost:9999 \
  TF_GOTRUE_JWKS_URL=http://localhost:9999/.well-known/jwks.json \
  TF_GOTRUE_ISSUER=https://triagefactory.yourcompany.com/auth/v1 \
  TF_PUBLIC_URL=https://triagefactory.yourcompany.com \
  ./triagefactory
```

(End-to-end multi-mode boot is not wired yet — see SKY-242 for the v1 epic. D6 brings up the auth substrate; D7 wires the handlers.)

## 5. Verify JWKS is reachable

```sh
curl -s http://localhost:9999/.well-known/jwks.json | jq .
```

You should see a JWKS containing **one RSA key, public side only** (`n` + `e`; no `d`, `p`, `q`). The `kid` matches what `jwk-init` produced.

## 6. Smoke-test the Verifier

Mint a test token via GoTrue's signup endpoint (no GitHub dance required):

```sh
TOKEN=$(curl -s -X POST http://localhost:9999/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"smoke@example.com","password":"smoketest123"}' \
  | jq -r .access_token)
```

Round-trip through the Verifier:

```sh
echo "$TOKEN" | TF_GOTRUE_JWKS_URL=http://localhost:9999/.well-known/jwks.json \
  TF_GOTRUE_ISSUER=${TF_PUBLIC_URL}/auth/v1 \
  ./triagefactory jwk-init --verify
```

You should see the parsed claims printed as JSON (`Subject`, `Email`, `Provider`, etc.).

## Rotating the signing key

1. Stop GoTrue: `docker compose stop gotrue`
2. Remove the existing `GOTRUE_JWT_KEYS=` line from `.env`
3. `./triagefactory jwk-init --write-env .env`
4. Restart GoTrue: `docker compose start gotrue`

The Verifier picks up the new key automatically on the next unknown-`kid` lookup — no TF restart needed.
