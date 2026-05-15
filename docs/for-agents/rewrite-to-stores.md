# Raw `db.*` → store-pattern migration (briefing)

You're picking up the mechanical follow-up to the SKY-246 (D2) store-abstraction wave. The codebase has 10 stores already; 9 more resources still use raw package-level `db.*` functions instead of going through a store interface. Your job is to migrate them, one resource at a time, **one PR per store**, mirroring the existing wave pattern.

This is **not multi-tenant correctness work** — SKY-253 (D9) handles that next. You're cleaning up the architectural inconsistency so D9 starts with a uniform codebase. Don't try to thread `orgID` into handlers or add session middleware — that's D9's job and will only conflict.

---

## Why this exists

The store abstraction (SKY-246) was designed so:

- Local mode (SQLite) and multi mode (Postgres) share a single handler-facing interface, with backend-specific impls hidden behind it.
- Stores can be wired against a `*sql.DB` (default) or a `*sql.Tx` (when composed inside `Stores.Tx.WithTx`) — handlers get atomicity without having to thread `*sql.Tx` themselves.
- Each resource interface takes `orgID` (and `userID` where relevant) explicitly on every method. Postgres impls include `org_id` in `WHERE` clauses as defense-in-depth alongside RLS. SQLite impls ignore `orgID` beyond asserting it equals `runmode.LocalDefaultOrg`.

D2 wave 0 (the pilot, ScoreStore) landed via SKY-246 wave 0 PR. Waves 1–4 landed PromptStore, SwipeStore, DashboardStore, EventHandlerStore, ChainStore, AgentStore, TeamAgentStore, UsersStore, SecretStore. These are your reference implementations.

What still uses raw `db.*` functions (109 exported functions across 9 files, ~150+ call sites):

| File | Funcs | Future store name | Approx call-site fanout |
|---|---|---|---|
| `internal/db/tasks.go` | 21 | `TaskStore` | high (tasks handler + router + scorer + delegate + carryover) |
| `internal/db/entities.go` | 14 | `EntityStore` | high (tracker + poller + curator + factory) |
| `internal/db/projects.go` | 5 | `ProjectStore` | moderate (projects handler + curator) |
| `internal/db/agent.go` | 24 | `AgentRunStore` | high (agent handler + spawner + chains) |
| `internal/db/reviews.go` | 12 | `ReviewStore` | moderate (reviews handler + spawner) |
| `internal/db/pending_prs.go` | 9 | `PendingPRStore` | moderate (pending_prs handler + spawner) |
| `internal/db/pending_firings.go` | 9 | `PendingFiringsStore` | low (router) |
| `internal/db/repos.go` | 9 | `RepoStore` | moderate (repos handler + settings + worktree) |
| `internal/db/factory.go` | 6 | `FactoryReadStore` (or fold into existing DashboardStore) | low (factory handler + lifetime_counter) |

---

## Required reading before you touch anything

1. **`docs/specs/sky-246-d2-store-abstraction.html`** — the authoritative spec for the pattern. If you find a conflict between this briefing and the spec, the spec wins; flag the conflict in the PR.
2. **Reference store: `ScoreStore`** — the cleanest small example.
   - Interface: `internal/db/scores.go`
   - Postgres impl: `internal/db/postgres/scores.go`
   - SQLite impl: `internal/db/sqlite/scores.go`
   - Postgres tests: `internal/db/postgres/scores_test.go`
   - SQLite tests: `internal/db/sqlite/scores_test.go`
   - Wiring: `internal/db/postgres/store.go` (`New` and `NewForTx`), `internal/db/sqlite/store.go`
3. **Reference store: `SwipeStore`** — a more involved example with multi-statement methods and a SQLite/Postgres divergence around audit semantics. Same file layout.
4. **`pkg/websocket/hub.go` is NOT in scope** — that's D9's WS retrofit; do not touch.
5. **CLAUDE.md** at repo root for build/lint conventions, branch naming, and the migration brick-check rule (you won't add any migrations).

---

## The pattern (one store, end to end)

For each resource, your migration looks like this:

### Step 1 — Define the interface

In a new file `internal/db/<resource>.go`:

```go
package db

import (
    "context"
    "github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TaskStore --output=./mocks --case=underscore --with-expecter

// TaskStore owns the tasks table — lifecycle, claims, swipe-triggered
// transitions. All methods take orgID; local mode passes
// runmode.LocalDefaultOrg.
type TaskStore interface {
    GetTask(ctx context.Context, orgID, taskID string) (*domain.Task, error)
    // ... etc
}
```

Match the doc-comment style of existing stores: lead with what the store owns, note the orgID convention, explain any non-obvious method (the `MarkScoring` doc in `scores.go` is a good template).

**Method shape rules:**
- Every method takes `ctx context.Context` as the first arg.
- Every method takes `orgID string` as the second arg.
- For per-user data, `userID string` is the third arg.
- Return domain types from `internal/domain`, not DB-shaped structs. If the existing raw function returned a DB-internal struct, fix that at the migration boundary.
- Error wrapping: `fmt.Errorf("store-op: %w", err)` per existing pattern.

**Don't duplicate methods.** If two raw functions are really the same operation with different argument orderings, fold them. If they're genuinely different paths, keep both.

### Step 2 — Add to the Stores bundle

In `internal/db/store.go`:

```go
type Stores struct {
    // ... existing fields
    Tasks TaskStore   // <- add this
}

type TxStores struct {
    // ... existing fields
    Tasks TaskStore   // <- add this too
}
```

### Step 3 — Write the Postgres impl

`internal/db/postgres/<resource>.go`:

```go
package postgres

import (
    "context"
    "database/sql"
    "github.com/sky-ai-eng/triage-factory/internal/db"
    "github.com/sky-ai-eng/triage-factory/internal/domain"
)

type taskStore struct{ q queryer }

func newTaskStore(q queryer) db.TaskStore { return &taskStore{q: q} }

var _ db.TaskStore = (*taskStore)(nil)

func (s *taskStore) GetTask(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
    // SQL written against the Postgres schema in
    // internal/db/migrations-postgres/202605130001_pg_baseline.sql.
    // org_id in every WHERE clause even though RLS would filter
    // anyway — defense in depth.
    var t domain.Task
    err := s.q.QueryRowContext(ctx, `
        SELECT ... FROM tasks WHERE org_id = $1 AND id = $2
    `, orgID, taskID).Scan(...)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    return &t, err
}
```

**Critical conventions:**
- Use the `queryer` interface (defined in `internal/db/postgres/queryer.go`) — both `*sql.DB` and `*sql.Tx` satisfy it, so the same impl runs inside or outside a transaction.
- `org_id = $1` in every WHERE clause. RLS does the same filter, but explicit is defense-in-depth.
- `sql.ErrNoRows` → `return nil, nil`, not an error. Existing stores do this; preserve the convention. Empty list returns `[]`, not `nil`.
- For multi-statement methods that need atomicity inside a single store method call, use the `inTx` helper if it exists in postgres/, or take a `*sql.Tx` explicitly (see SwipeStore for the pattern).

**Where to wire:**
- `internal/db/postgres/store.go` — add to `New` (line ~56) for the non-tx wiring and `NewForTx` (line ~127) for the tx wiring.
- `internal/db/postgres/tx.go` — add to the `txStores` struct populated inside `WithTx`.

**Admin vs app pool:**
- TaskStore, ProjectStore, AgentRunStore, ReviewStore, PendingPRStore, RepoStore, EntityStore: **app pool** (RLS-active). Per-user request data.
- PendingFiringsStore, FactoryReadStore: **admin pool**. System-level reads (router, scorer, factory snapshot).
- Look at how existing stores decide (`scoreStore` is admin, `agentStore.GetForOrg` is app) — pattern is in the doc comments at the top of each existing Postgres impl.

### Step 4 — Write the SQLite impl

`internal/db/sqlite/<resource>.go`:

Same shape as Postgres but adapted to SQLite's schema (which doesn't have `org_id` columns — local mode is single-tenant by design). The SQLite impl asserts `orgID == runmode.LocalDefaultOrg` at the top of each method and then runs the query without the org filter.

```go
func (s *taskStore) GetTask(ctx context.Context, orgID, taskID string) (*domain.Task, error) {
    if orgID != runmode.LocalDefaultOrg {
        return nil, fmt.Errorf("sqlite tasks: unexpected orgID %q", orgID)
    }
    // SQL against the SQLite schema in
    // internal/db/migrations-sqlite/<latest>.sql
    var t domain.Task
    err := s.q.QueryRowContext(ctx, `SELECT ... FROM tasks WHERE id = ?`, taskID).Scan(...)
    if err == sql.ErrNoRows {
        return nil, nil
    }
    return &t, err
}
```

Wiring:
- `internal/db/sqlite/store.go` — add to `New`.
- `internal/db/sqlite/tx.go` — add to the `txStores` struct.

### Step 5 — Tests

Two test files per store: `internal/db/postgres/<resource>_test.go` and `internal/db/sqlite/<resource>_test.go`.

**Postgres tests** use the `pgtest.Harness` (`internal/db/pgtest/`). They:
- Skip cleanly when Docker isn't available (`pgtest.Shared(t)` handles this — see existing tests for the pattern).
- Seed test data through the harness's admin DB connection (BYPASSRLS), then exercise the store via the app pool to validate RLS behavior end-to-end.
- Cover the happy path, the empty-result path, and the cross-org-leakage path (User A in Org A can't see Org B's row).

**SQLite tests** use the `dbtest` helpers (or whichever helper the existing SQLite tests use — check `internal/db/sqlite/scores_test.go`). Cover happy path + empty path. Cross-org doesn't apply.

Don't write golden tests for SQL strings. Test behavior, not the query text.

### Step 6 — Migrate every caller

`grep -rn "db\.<FunctionName>" internal/` for each function the store now owns. Each call site moves from:

```go
task, err := db.GetTask(s.db, taskID)
```

to:

```go
task, err := s.stores.Tasks.GetTask(ctx, orgID, taskID)
```

For handlers, `orgID` comes from `OrgIDFrom(r.Context())` *if D9 has landed already* — but it hasn't yet at the time you're working, so handlers will pass `runmode.LocalDefaultOrg` for now. **That's fine; D9 retrofits handlers anyway.** Your job is to swap the call shape; D9 swaps the value.

For non-handler callers (poller, router, scorer, delegate spawner): they each have a notion of "current org" already — most pass `runmode.LocalDefaultOrg`. Preserve whatever they pass today. Don't change call-site semantics; only the call shape.

**The Server constructor:** `internal/server/server.go::New` already takes per-store fields. Add the new store. `main.go` constructs the Stores bundle and passes individual fields through. You'll update both.

### Step 7 — Delete the old raw functions

Once every call site is migrated, the package-level `db.<FunctionName>` functions are dead. Delete them. Keep helper types (`HandoffResult`, etc.) where they're still referenced by the new store.

Some raw functions are shared utility — e.g. a scan helper used by multiple stores. Either lift them to `internal/db/<resource>_scan.go` shared between the SQLite and Postgres impls, or duplicate per-backend. The existing pattern in `scores.go` is per-backend duplication with a TODO to extract; follow that.

### Step 8 — Build + lint + test

```bash
cd frontend && npm install && npm run build && cd ..
go build -o ./triagefactory .
./scripts/lint.sh
go test ./...
```

All must be green. The pgtest harness will spin up a Postgres testcontainer; that's fine, leave it.

---

## PR shape

**One PR per store.** Branch naming: `aa/sky-<ticket-of-the-day>-<store-name>` — if a Linear ticket already exists for that store, use its number; if not, no ticket reference needed in the branch name. (Don't create new Linear tickets unless explicitly told to.)

**Order of PRs (call-site count descending — biggest impact first):**

1. **TaskStore** (`tasks.go`, 21 functions) — most-called, anchors the pattern
2. **EntityStore** (`entities.go`, 14 functions)
3. **AgentRunStore** (`agent.go`, 24 functions) — large but contained
4. **ReviewStore** (`reviews.go`, 12 functions)
5. **PendingPRStore** (`pending_prs.go`, 9 functions)
6. **RepoStore** (`repos.go`, 9 functions)
7. **PendingFiringsStore** (`pending_firings.go`, 9 functions)
8. **ProjectStore** (`projects.go`, 5 functions)
9. **FactoryReadStore** (`factory.go`, 6 functions) — possibly folded into DashboardStore; decide on the spot based on whether the methods make sense alongside existing dashboard methods

**Each PR is independent — based on `main`, not on the previous PR.** Don't chain branches. If two stores would otherwise touch the same caller (e.g. handleAgentMessages reads tasks + agent runs), pick the merge order such that the second store's PR rebases cleanly. The order above is designed around this.

**Per-PR commit message:**
```
feat(db): SKY-246 wave N — <StoreName> for both backends

Migrates internal/db/<resource>.go's raw db.* functions to a
<StoreName> interface + Postgres/SQLite impls. Every method takes
orgID + (where relevant) userID. Defense-in-depth org_id filters
in Postgres alongside RLS; SQLite asserts orgID == LocalDefaultOrg.

Call sites in <list-of-touched-packages> migrated. Old raw functions
deleted.

Co-Authored-By: <agent-id>
```

**PR description template** — keep tight:
```
## Summary
- Adds <StoreName> interface (internal/db/<resource>.go)
- Postgres + SQLite impls, both with tests
- Migrates N call sites
- Deletes the old internal/db/<resource>.go raw functions

## Test plan
- [x] go test ./...
- [x] ./scripts/lint.sh
- [x] go build -o ./triagefactory .
- [x] Cross-org leakage test (Postgres only — N/A in local mode)
- [ ] Manual smoke if any caller is on a UI-visible path (call out which)
```

---

## What NOT to do

1. **Don't migrate to org-scoped routing.** That's D9 (SKY-253). Handler URLs stay flat. Middleware stays as-is.
2. **Don't touch `pkg/websocket/`.** D9 owns the WS retrofit.
3. **Don't add new RLS policies.** They already exist for all 39 org-scoped tables. If you find a missing policy, flag it in the PR description; do not silently add one.
4. **Don't add migrations.** The brick check at `internal/db/migrations.go` rejects pre-v1.11.0 installs and the baseline is the source of truth. Schema is frozen for this work.
5. **Don't bundle stores into one PR.** One per store. The reviewer wants kill-switches between them.
6. **Don't introduce new domain types.** If `internal/domain/` doesn't already model what your store returns, the raw function's existing shape is the source of truth — match it.
7. **Don't add features.** No new methods on the stores that aren't 1:1 with an existing raw function. Pure migration.
8. **Don't refactor unrelated handler logic.** If a handler does something weird, leave it weird. SKY-253 / future tickets handle handler-level cleanups.
9. **Don't skip the SQLite impl.** Local mode is a real deployment; "we're going multi-mode anyway" is not a valid reason to leave the SQLite side out. Tests for both backends must pass.
10. **Don't merge without the reviewer's approval.** Wait for codex / ultrareview / human sign-off per repo conventions. If CI flakes, retry the failing job, don't `--no-verify`.

---

## Loose ends to flag, not fix

If you encounter any of these, mention them in the PR description but do not address:

- **Inconsistent scan helpers** between SQLite and Postgres for the same domain type. (Existing TODO; extract is wave 3a-shaped, not your concern.)
- **Functions that look like they belong on a different store** (e.g. a "task" function that's really a "swipe"). Flag in PR; don't re-home unless trivial.
- **Mocks regenerate** — `mockery` should be re-run for the new store. If `mockery` isn't installed/configured locally, the `go:generate` comment in the interface file is enough; flag that the mocks need regeneration on first use.
- **`internal/db/withtx.go` (the lower-level WithTx wrapper)** — D9 will probably consolidate the two WithTx flavors. Don't touch it.

---

## Definition of done (for the whole arc)

- All 9 stores live with both backend impls + tests
- Zero exported functions remaining in `internal/db/tasks.go`, `agent.go`, `entities.go`, `reviews.go`, `pending_prs.go`, `pending_firings.go`, `repos.go`, `projects.go`, `factory.go` (or those files deleted entirely if no shared helpers remain)
- `grep -rn "db\.\(GetTask\|FindOrCreate\|BumpTask\|...\)" internal/` returns nothing in handler/poller/spawner files
- Full test suite green
- Frontend build + lint green (you shouldn't be touching FE, but verify)
- 9 PRs merged to main

When the last PR lands, drop a one-line summary on this file (append, don't replace) with the PR numbers in order. Then notify the human — D9 picks up from there.
