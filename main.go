package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/install"
	"github.com/sky-ai-eng/triage-factory/cmd/migrate"
	"github.com/sky-ai-eng/triage-factory/cmd/resume"
	"github.com/sky-ai-eng/triage-factory/cmd/uninstall"
)

const defaultPort = 3000

// Version is the binary's release tag, set by the linker at build time
// (`-ldflags "-X main.Version=v0.1.0"`). Local builds without that flag
// see the literal "dev" so anything in the wild claiming to be "dev" is
// known to be unreleased.
var Version = "dev"

// pluralize picks the singular or plural form of a noun based on count.
// Used for toast copy where "1 entity tracked" vs "5 entities tracked"
// reads nicer than a naive "(s)" suffix.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// bootstrapBareClones reads the configured repos from the DB and asks
// the worktree package to ensure each one is materialized on disk
// as a bare clone with the right origin URL.
//
// Called after profiling completes — profiling is what populates
// repo_profiles.clone_url, and BootstrapTargets without a CloneURL
// are skipped. Profiles never become non-empty without a successful
// profiling pass, so this ordering is intentional.
//
// Database read errors are logged and the bootstrap is skipped: a
// transient DB issue shouldn't crash the main path, and the lazy
// clone inside CreateForPR / CreateForBranch will recover the
// affected delegations on next run.
func bootstrapBareClones(database *sql.DB) {
	profiles, err := db.GetAllRepoProfiles(database)
	if err != nil {
		log.Printf("[worktree] bootstrap: load profiles: %v", err)
		return
	}
	targets := make([]worktree.BootstrapTarget, 0, len(profiles))
	for _, p := range profiles {
		targets = append(targets, worktree.BootstrapTarget{
			Owner:    p.Owner,
			Repo:     p.Repo,
			CloneURL: p.CloneURL,
		})
	}
	worktree.BootstrapBareClones(context.Background(), targets)
}

// bootstrapLocalGitHubIdentity populates users.github_username on the
// local synthetic user row by deriving the login from the configured
// PAT+URL. Runs at startup before seedDefaultPrompts so the SQLite
// Seed substitution sees the populated value when it wires
// author_in/reviewer_in/commenter_in allowlists into shipped event
// handler predicates.
//
// No-op when (a) the column already has a value, (b) credentials are
// absent (Settings UI capture is the alternate write path), or
// (c) ValidateGitHub fails (PAT invalid / GitHub down — the user
// can recapture via Settings, or the next boot retries).
func bootstrapLocalGitHubIdentity(users db.UsersStore) error {
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}
	ctx := context.Background()

	creds, _ := auth.Load() // auth errors are non-fatal — degrade to no-op
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		return nil
	}
	existing, err := users.GetGitHubUsername(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		return fmt.Errorf("read users.github_username: %w", err)
	}
	if existing != "" {
		return nil
	}
	ghUser, err := auth.ValidateGitHub(creds.GitHubURL, creds.GitHubPAT)
	if err != nil {
		log.Printf("[bootstrap] derive users.github_username from PAT: %v (continuing — Settings will capture next save)", err)
		return nil
	}
	if err := users.SetGitHubUsername(ctx, runmode.LocalDefaultUserID, ghUser.Login); err != nil {
		return fmt.Errorf("persist users.github_username: %w", err)
	}
	log.Printf("[bootstrap] users.github_username: derived %q from credentials", ghUser.Login)
	return nil
}

// printTopLevelHelp routes the two audiences (delegated Claude Code
// agents vs. human users) to the right surface. Agents almost always
// reach this through autocomplete / accidental invocation when they
// were trying to run a scoped subcommand, so the first thing they
// should see is the `exec` pointer; humans typically want the server
// flags and the takeover-resume shortcuts. Keep it short — anything
// longer goes in docs/usage.md, which we link to.
func printTopLevelHelp() {
	fmt.Println(`triagefactory — local-first AI triage for engineering backlogs.

Run with no arguments to start the server (port 3000, opens browser).

USER COMMANDS
  triagefactory                            start the server
  triagefactory --port N                   start on a custom port
  triagefactory --no-browser               start without opening a browser
  triagefactory --version                  print the binary's version
  triagefactory install [--dest <path>]    symlink the binary onto PATH
  triagefactory uninstall [--yes]          wipe local state (db, config,
                                           keychain, takeovers); leaves
                                           the binary itself in place
  triagefactory resume [<short-id>]        resume a taken-over session
                                           (auto-resumes when there's only
                                           one; picker otherwise)
  triagefactory migrate up                 bring the schema to head
  triagefactory migrate status             list applied + pending migrations

AGENT COMMANDS
  Used by delegated Claude Code agents inside their worktree, not
  meant for direct invocation by humans.

  triagefactory exec <subcommand> ...      scoped GitHub / Jira ops
                                           (run "triagefactory exec --help"
                                           for the full list)
  triagefactory status <run-id>            check a delegated run's status

For configuration, polling, and feature details, see docs/usage.md.`)
}

func main() {
	// Initialize the runtime mode flag (TF_MODE env, default local)
	// as the first thing the binary does — every dispatched
	// subcommand below runs after this so the package-level mode is
	// set before any subsystem touches a path or opens a DB. SKY-248
	// (D4a) only ships the mode flag; D4b adds the path resolvers
	// that consume it (under a separate internal/paths package).
	if err := runmode.InitFromEnv(); err != nil {
		log.Fatalf("runmode: %v", err)
	}

	// Dual-mode dispatch:
	//   exec/status — CLI-only, used by Claude Code agent.
	//   resume      — user-facing, hands the terminal back into a
	//                 previously taken-over Claude Code session.
	//   install     — user-facing, symlinks the binary onto PATH so
	//                 `triagefactory resume` works without a full path.
	//   uninstall   — user-facing, wipes everything install + the server
	//                 leave behind on the host (db, config, keychain,
	//                 takeover dirs). Doesn't remove the binary itself.
	//   -h/--help   — top-level usage; the help text routes the two
	//                 audiences (delegated agents vs human users) to
	//                 the right surface.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			exec.Handle(os.Args[2:])
			return
		case "status":
			exec.HandleStatus(os.Args[2:])
			return
		case "resume":
			resume.Handle(os.Args[2:])
			return
		case "install":
			install.Handle(os.Args[2:])
			return
		case "uninstall":
			uninstall.Handle(os.Args[2:])
			return
		case "migrate":
			migrate.Handle(os.Args[2:])
			return
		case "-h", "--help", "help":
			printTopLevelHelp()
			return
		case "-v", "--version", "version":
			fmt.Println(Version)
			return
		}
	}

	// Server mode: start HTTP server + pollers
	port := defaultPort
	noBrowser := false

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--port":
			if i+1 < len(os.Args) {
				p, err := strconv.Atoi(os.Args[i+1])
				if err != nil {
					log.Fatalf("invalid port: %s", os.Args[i+1])
				}
				port = p
				i++
			}
		case "--no-browser":
			noBrowser = true
		}
	}

	// Runmode dispatch (SKY-246 D2 wave 0): open the right backend
	// for the mode and wire the per-resource store bundle against
	// it. The dispatch wraps db.Open/db.Migrate so a misconfigured
	// TF_MODE=multi fails fast — without this guard, the local
	// SQLite file at ~/.triagefactory/triagefactory.db would be
	// created and migrated before the multi branch could reject.
	//
	// Multi mode is unreachable end-to-end until the v1 multi-tenant
	// epic (SKY-242) completes: every store needs to migrate to the
	// per-resource interface, and D7 needs to wire the Postgres
	// connection config. Until then, packages outside the converted
	// stores still call db.X(*sql.DB, ...) helpers that emit SQLite
	// SQL — pointed at Postgres they'd produce runtime errors. The
	// fatal here makes the unreachable state explicit instead of
	// surfacing later as a pile of confusing SQL failures.
	var (
		database *sql.DB
		stores   db.Stores
	)
	switch runmode.Current() {
	case runmode.ModeLocal:
		var err error
		database, err = db.Open()
		if err != nil {
			log.Fatalf("failed to open database: %v", err)
		}
		if err := db.Migrate(database, "sqlite3"); err != nil {
			log.Fatalf("failed to migrate database: %v", err)
		}
		// Fail fast if the migration's seeded UUIDs drifted from the
		// runmode constants — every team_id/creator_user_id DEFAULT
		// clause in the SQLite baseline embeds these literally, so a
		// mismatch would silently produce orphan rows.
		if err := db.AssertLocalSentinels(database); err != nil {
			log.Fatalf("%v", err)
		}
		stores = sqlitestore.New(database)
	case runmode.ModeMulti:
		log.Fatalf("TF_MODE=multi: multi-tenant mode is not yet wired end-to-end; see SKY-242 (v1 multi-tenant epic). Unset TF_MODE to run in local mode.")
	default:
		log.Fatalf("unknown runmode: %v", runmode.Current())
	}
	defer database.Close()

	// Wire the config package against the DB. Must run after Migrate
	// (settings table is created there) and before any config.Load /
	// Save call. Fresh installs land at Default() on first Load() and
	// write to the settings row on first Save().
	if err := config.Init(database); err != nil {
		log.Fatalf("failed to initialize config: %v", err)
	}

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Triage Factory running at http://localhost%s\n", addr)

	// One-shot PATH hint. The `triagefactory resume` subcommand only
	// works from any terminal once the binary's on PATH; nudge the
	// user toward `triagefactory install` if it isn't. Best-effort.
	install.HintIfMissing()

	if !noBrowser {
		openBrowser(fmt.Sprintf("http://localhost%s", addr))
	}

	srv := server.New(database, stores.Prompts, stores.Swipes, stores.Dashboard, stores.EventHandlers, stores.Agents, stores.TeamAgents, stores.Users, stores.Chains)

	distFS, err := frontendDist()
	if err != nil {
		log.Fatalf("failed to load embedded frontend: %v", err)
	}
	srv.SetStatic(distFS)

	// Clean up any orphaned worktrees from crashed runs. taken_over runs
	// are preserved at the ~/.claude/projects level so the user can still
	// resume their takeover sessions after a binary restart.
	//
	// On query error we still sweep worktree dirs and prune bare repos
	// (those leaks compound fast — each can be GBs), but skip ALL
	// ~/.claude/projects deletions: without the preserve set we can't
	// distinguish a taken-over run's session JSONL from a regular
	// orphan, and silently nuking a JSONL would break the user's ability
	// to resume.
	preserveIDs, err := db.ListTakenOverRunIDs(database)
	if err != nil {
		log.Printf("[server] WARNING: failed to load taken_over run ids — sweeping worktree dirs but skipping ~/.claude/projects cleanup to avoid clobbering active takeover sessions: %v", err)
		worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
	} else {
		preserveSet := make(map[string]bool, len(preserveIDs))
		for _, id := range preserveIDs {
			preserveSet[id] = true
		}
		worktree.CleanupWithOptions(worktree.CleanupOptions{PreserveClaudeProjectFor: preserveSet})
	}

	// events_catalog is seeded by the v1.11.0 baseline migration in both
	// backends — no boot-time seed call needed. New event types ship via
	// a new forward migration. Prompts are seeded inside seedDefaultPrompts
	// before EventHandlers.Seed runs so the FK from event_handlers.prompt_id
	// → prompts.id resolves on the trigger rows.
	//
	// Populate users.github_username before seeding event handlers so the
	// SQLite Seed substitution sees the local user's login when it wires
	// allowlist placeholders on shipped predicates.
	if err := bootstrapLocalGitHubIdentity(stores.Users); err != nil {
		log.Printf("[bootstrap] users.github_username: %v (continuing — Settings will capture on next save)", err)
	}
	seedDefaultPrompts(stores.Prompts, stores.EventHandlers)

	// Bootstrap the local-mode agent identity (SKY-260 D-Agent). One
	// agents row + one team_agents row for the synthetic LocalDefaultOrg
	// / LocalDefaultTeamID pair. Idempotent (INSERT OR IGNORE) — re-runs
	// across boots leave existing rows intact, preserving any user-
	// disable on team_agents.enabled.
	//
	// Fatal on failure: post-SKY-261 the agents row is load-bearing for
	// the entire claim flow (stampAgentClaim's GetForOrg, the drain
	// path's claim_changed guard, runs.actor_agent_id stamping). The
	// idempotent INSERT means the only legitimate failure mode is a
	// DB connection issue — and Migrate() above already fatals on
	// that. Continuing past a bootstrap failure produces a silently-
	// broken auto-delegation state where the user wouldn't see an
	// error, just notice things never fire. Better to surface the
	// failure at startup.
	if err := db.BootstrapLocalAgent(context.Background(), stores); err != nil {
		log.Fatalf("[bootstrap] local agent: %v (auto-delegation depends on this; refusing to start)", err)
	}

	// Auto-import Claude Code skill files as prompts. context.Background
	// at startup — no request to inherit from, the import runs to
	// completion or fails of its own accord.
	skills.ImportAll(context.Background(), database, stores.Prompts)

	// Event bus — central pub/sub replacing direct callbacks
	bus := eventbus.New()

	wsHub := srv.WSHub()

	// Wire the worktree clone-result callback before any bootstrap or
	// lazy-clone path can fire. EnsureBareClone (and its private
	// equivalent used by CreateForPR / createBranchWorktreeAt) invokes
	// this on every attempt; we use it to stamp repo_profiles with the
	// outcome and broadcast a websocket event so the Repos page updates
	// live. Failures get an SSH preflight to classify whether the SSH
	// side is the cause — that drives the per-row CTA on the frontend
	// ("Fix in Settings" for SSH issues, raw stderr otherwise).
	worktree.SetOnCloneResult(func(owner, repo string, cloneErr error) {
		if cloneErr == nil {
			if err := db.UpdateRepoCloneStatus(database, owner, repo, "ok", "", ""); err != nil {
				log.Printf("[clone-status] update %s/%s ok: %v", owner, repo, err)
			}
			wsHub.Broadcast(websocket.Event{
				Type: "repo_profile_updated",
				Data: map[string]any{
					"id":           owner + "/" + repo,
					"clone_status": "ok",
				},
			})
			return
		}

		log.Printf("[clone-status] %s/%s clone failed: %v", owner, repo, cloneErr)

		kind := "other"
		if cfg, cErr := config.Load(); cErr == nil && cfg.GitHub.CloneProtocol == "ssh" {
			// Use the configured GitHub host so GHE installs probe
			// the right SSH endpoint, not github.com. Falls back to
			// git@github.com when the URL is empty/unparseable.
			creds, _ := auth.Load()
			sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if perr := worktree.CachedPreflightSSH(ctx, sshHost); perr != nil {
				kind = "ssh"
				log.Printf("[clone-status] %s/%s SSH preflight against %s also failed → kind=ssh: %v", owner, repo, sshHost, perr)
			} else {
				log.Printf("[clone-status] %s/%s SSH preflight against %s passed → kind=other (clone error is on the git side)", owner, repo, sshHost)
			}
			cancel()
		} else if cErr != nil {
			log.Printf("[clone-status] %s/%s load config to classify: %v (defaulting to kind=other)", owner, repo, cErr)
		}

		if err := db.UpdateRepoCloneStatus(database, owner, repo, "failed", cloneErr.Error(), kind); err != nil {
			log.Printf("[clone-status] update %s/%s failed: %v", owner, repo, err)
		}
		wsHub.Broadcast(websocket.Event{
			Type: "repo_profile_updated",
			Data: map[string]any{
				"id":               owner + "/" + repo,
				"clone_status":     "failed",
				"clone_error":      cloneErr.Error(),
				"clone_error_kind": kind,
			},
		})
	})

	// Lifetime distinct-entity counter for the factory snapshot. Hydrate
	// once from the events table so we don't pay a full-table scan per
	// /api/factory/snapshot request, then keep it warm via the
	// db.SetOnEventRecorded hook — which fires inside RecordEvent itself
	// so direct callers (tracker backfill, Jira carry-over) that skip
	// the eventbus still update the cache. Hydrate must complete before
	// the hook is wired so a fresh event can't land in the dedupe set
	// ahead of the historical scan.
	lifetimeCounter := db.NewLifetimeDistinctCounter()
	if err := lifetimeCounter.Hydrate(database); err != nil {
		log.Fatalf("hydrate lifetime counter: %v", err)
	}
	srv.SetLifetimeCounter(lifetimeCounter)
	db.SetOnEventRecorded(lifetimeCounter.Record)

	// Subscriber: WS broadcaster — forwards ALL events to the frontend
	bus.Subscribe(eventbus.Subscriber{
		Name: "ws-broadcast",
		Handle: func(evt domain.Event) {
			wsHub.Broadcast(websocket.Event{
				Type: "event",
				Data: evt,
			})
			// Also send the legacy "tasks_updated" for backward compat
			if evt.EventType == domain.EventSystemPollCompleted {
				wsHub.Broadcast(websocket.Event{
					Type: "tasks_updated",
					Data: map[string]any{},
				})
			}
		},
	})

	// Start AI scoring runner
	// Profile gate — scorer waits for this before running
	profileGate := repoprofile.NewProfileGate(database)

	// Declare eventRouter early so the scorer callback can reference it.
	// Actual initialization happens below after the spawner is created.
	var eventRouter *routing.Router

	scorer := ai.NewRunner(database, stores.Scores, runmode.LocalDefaultOrg, ai.RunnerCallbacks{
		OnScoringStarted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_started",
				Data: map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type: "scoring_completed",
				Data: map[string]any{"task_ids": taskIDs},
			})
			// Post-scoring re-derive: check deferred triggers whose
			// min_autonomy_suitability threshold the scored tasks now meet.
			// Runs async so it doesn't block the scorer from clearing its
			// running flag and handling subsequent Trigger() calls.
			if eventRouter != nil {
				go eventRouter.ReDeriveAfterScoring(taskIDs)
			}
		},
		OnTasksSkipped: func(skipped, total int) {
			toast.Warning(wsHub, fmt.Sprintf("AI scoring: %d of %d tasks skipped this cycle", skipped, total))
		},
		OnError: func(err error) {
			toast.Error(wsHub, fmt.Sprintf("AI scoring cycle aborted: %v", err))
		},
	})
	scorer.SetProfileGate(profileGate.Ready)
	scorer.Start()
	srv.SetScorerTrigger(scorer.Trigger)
	log.Println("[ai] scorer started (model: haiku)")

	// Subscriber: scorer trigger — only reacts to poll-complete sentinels
	bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			scorer.Trigger()
		},
	})

	// Project classifier (SKY-220): per-poll, classify any newly-
	// discovered entities against existing projects via per-project
	// Haiku quorum vote. Sticky — only fires on entities with
	// classified_at IS NULL, so re-polls don't re-classify.
	classifier := projectclassify.NewRunner(database)
	classifier.Start()
	log.Println("[classify] project classifier started (model: haiku)")
	bus.Subscribe(eventbus.Subscriber{
		Name:   "classifier",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			classifier.Trigger()
		},
	})

	// Poller manager — uses event bus instead of direct callbacks.
	// Poll errors are toasted with per-source time-based throttling: the
	// poller fires OnError on every failure (raw signal), but we only
	// refresh the user-facing toast every errorToastMinInterval. Without
	// throttling, a persistent failure (expired PAT, network outage) would
	// generate a sticky error toast every poll cycle (default 5m) until
	// the user manually dismissed each one — badly spammy on the UI.
	const errorToastMinInterval = 5 * time.Minute
	var (
		errorThrottleMu sync.Mutex
		lastErrorToast  = map[string]time.Time{}
	)
	pollerMgr := poller.NewManager(database, bus, stores.Users)
	pollerMgr.OnError = func(source string, err error) {
		errorThrottleMu.Lock()
		if last, ok := lastErrorToast[source]; ok && time.Since(last) < errorToastMinInterval {
			errorThrottleMu.Unlock()
			return
		}
		lastErrorToast[source] = time.Now()
		errorThrottleMu.Unlock()

		label := "Jira"
		if source == "github" {
			label = "GitHub"
		}
		toast.ErrorTitled(wsHub, label, fmt.Sprintf("Poll failed: %v", err))
	}

	// Create spawner once — credentials are hot-swapped in place
	spawner := delegate.NewSpawner(database, stores.Prompts, stores.Agents, stores.Chains, nil, wsHub, "")
	srv.SetSpawner(spawner)

	// SKY-220: wire the classifier wait into the spawner's setup path.
	// Before reading entity.project_id for KB injection, the spawner
	// blocks until classified_at is set (or DefaultWaitTimeout elapses).
	// projectclassify.WaitFor triggers the runner on entry to wake it up
	// even if no post-poll cycle has fired for this entity yet.
	spawner.SetWaitForClassification(func(ctx context.Context, entityID string) {
		projectclassify.WaitFor(ctx, database, classifier, entityID, projectclassify.DefaultWaitTimeout)
	})

	// Curator runtime (SKY-216) — per-project chat sessions. Any
	// rows left non-terminal from a previous process are stranded:
	// running rows lost their goroutine + agentproc subprocess,
	// queued rows lost the goroutine that was supposed to pick them
	// up. Cancel both classes so the user can re-send if they
	// actually wanted that message processed. Auto-replaying a
	// stale queued message after a restart would surprise the user
	// more than dropping it. The model arg is empty until config
	// loads; curator.UpdateCredentials hot-swaps the same way
	// Spawner does.
	if n, err := db.CancelOrphanedNonTerminalCuratorRequests(database); err != nil {
		log.Printf("[curator] startup recovery failed: %v", err)
	} else if n > 0 {
		log.Printf("[curator] cancelled %d orphaned non-terminal curator requests from prior process", n)
	}
	curatorRuntime := curator.New(database, stores.Prompts, wsHub, "")
	srv.SetCurator(curatorRuntime)

	// Knowledge-base file watcher — fires `project_knowledge_updated`
	// over the websocket whenever the curator (or anything else)
	// touches a file under <projectsRoot>/<id>/knowledge-base/. The
	// frontend Knowledge panel listens and refetches, so files appear
	// in the UI as the agent writes them mid-turn. Failure here is
	// non-fatal — the panel still works, just without live updates.
	if root, err := curator.ProjectsRoot(); err != nil {
		log.Printf("[kbwatcher] resolve projects root: %v (live KB updates disabled)", err)
	} else if _, err := curator.NewKnowledgeWatcher(wsHub, root); err != nil {
		log.Printf("[kbwatcher] start: %v (live KB updates disabled)", err)
	}

	// Event router — records events, creates/bumps tasks, auto-delegates on
	// matching triggers, runs inline close checks. Also handles post-scoring
	// re-derive via the scorer callback wired above.
	eventRouter = routing.NewRouter(database, stores.Prompts, stores.EventHandlers, stores.Agents, stores.TeamAgents, spawner, scorer, wsHub)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "router",
		Filter: []string{"github:", "jira:"},
		Handle: eventRouter.HandleEvent,
	})

	// Wire the queue drainer. Spawner calls router.DrainEntity from each
	// auto-run terminal so queued firings progress without their own
	// trigger event. Has to be set post-construction because router and
	// spawner reference each other (spawner.Delegate ← router; router.
	// DrainEntity ← spawner). Same pattern UpdateCredentials uses.
	spawner.SetQueueDrainer(eventRouter)

	// Periodic drain sweeper — safety net for queues stuck on transient
	// validation/fire errors. notifyDrainer only triggers drains on
	// auto-run terminals; if nothing's running, nothing wakes up the
	// queue. The sweep tick re-attempts pending firings every 30s.
	// Background context: the binary doesn't have a top-level cancel
	// today, so the goroutine lives for the process lifetime.
	go eventRouter.RunDrainSweeper(context.Background(), 30*time.Second)

	// Tracks per-source "announce next poll completion as a toast". Set when
	// a config change triggers a poller restart; cleared after the first
	// post-restart completion fires the toast. Prevents every-minute spam
	// while still giving users explicit feedback that their config took
	// effect.
	var (
		announceMu      sync.Mutex
		announcePending = map[string]bool{}
	)
	setAnnouncePending := func(source string) {
		announceMu.Lock()
		announcePending[source] = true
		announceMu.Unlock()
	}
	shouldAnnounce := func(source string) bool {
		announceMu.Lock()
		defer announceMu.Unlock()
		if announcePending[source] {
			announcePending[source] = false
			return true
		}
		return false
	}

	// GitHub changed: invalidate profiles → stop all → re-profile → restart all
	srv.SetOnGitHubChanged(func() {
		log.Println("[server] GitHub config changed, full restart...")
		setAnnouncePending("github")
		setAnnouncePending("jira")

		// Don't Invalidate the profile gate. Scoring no longer reads
		// repo profiles (LLM repo-match was removed when lazy Jira
		// worktrees landed), so the gate has no consumer; flipping it
		// back to false in the GitHub-disabled branch — which doesn't
		// Signal again — would silently freeze scoring forever.
		pollerMgr.StopAll()

		cfg, _ := config.Load()
		creds, _ := auth.Load()

		if cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner.UpdateCredentials(ghClient, cfg.AI.Model)
			curatorRuntime.UpdateCredentials(cfg.AI.Model)
			srv.SetGitHubClient(ghClient)

			// Re-profile, then signal ready and restart all pollers
			go func() {
				profiler := repoprofile.NewProfiler(ghClient, database, wsHub)
				repos, _ := db.GetConfiguredRepoNames(database)
				if err := profiler.Run(context.Background(), repos, true); err != nil {
					log.Printf("[repoprofile] profiling failed: %v", err)
				}
				profileGate.Signal()
				pollerMgr.RestartAll()
				scorer.Trigger()
				bootstrapBareClones(database)
			}()
		} else {
			spawner.UpdateCredentials(nil, "")
			curatorRuntime.UpdateCredentials("")
			srv.SetGitHubClient(nil)
			pollerMgr.RestartAll()
		}

		// Also refresh Jira client in case it's configured
		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
		} else {
			srv.SetJiraClient(nil)
		}
	})

	// Jira changed: restart only the Jira poller
	srv.SetOnJiraChanged(func() {
		log.Println("[server] Jira config changed, restarting Jira poller...")
		setAnnouncePending("jira")

		creds, _ := auth.Load()

		pollerMgr.RestartJira()

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
		} else {
			srv.SetJiraClient(nil)
		}
	})

	// Subscriber: track Jira/GitHub poll completions.
	// Jira: gates /api/jira/stock so it knows when snapshots are ready.
	// Both: surface a one-shot "first poll complete after config change"
	// toast so users can see their settings change actually took effect.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "poll-tracker",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			if evt.EventType != domain.EventSystemPollCompleted {
				return
			}
			var meta struct {
				Source    string `json:"source"`
				StartedAt int64  `json:"started_at"`
				Entities  int    `json:"entities"`
			}
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
				log.Printf("[poll-tracker] warning: failed to parse poll completion metadata: %v; raw metadata=%q", err, evt.MetadataJSON)
				return
			}
			if meta.Source == "jira" {
				// Pass the poll's started_at so MarkJiraPollComplete can ignore
				// stale sentinels from pre-restart poll goroutines that finish
				// late — RestartJira doesn't cancel in-flight RefreshJira calls.
				// A missing field yields StartedAt=0; pass a zero time.Time so
				// MarkJiraPollComplete treats it as "unknown generation" and
				// accepts it rather than getting stuck on {status:"polling"}.
				var startedAt time.Time
				if meta.StartedAt != 0 {
					startedAt = time.Unix(0, meta.StartedAt)
				}
				srv.MarkJiraPollComplete(startedAt)
			}
			if shouldAnnounce(meta.Source) {
				label := "GitHub"
				if meta.Source == "jira" {
					label = "Jira"
				}
				toast.Info(wsHub, fmt.Sprintf(
					"First %s poll complete — %d %s tracked",
					label, meta.Entities, pluralize(meta.Entities, "entity", "entities"),
				))
			}
		},
	})

	// Initial start with current credentials
	cfg, _ := config.Load()
	creds, _ := auth.Load()
	repoCount, _ := db.CountConfiguredRepos(database)

	if cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) && repoCount > 0 {
		ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
		spawner.UpdateCredentials(ghClient, cfg.AI.Model)
		curatorRuntime.UpdateCredentials(cfg.AI.Model)
		srv.SetGitHubClient(ghClient)
		log.Printf("[delegate] spawner ready (%d repos configured)", repoCount)

		// Profile repos, then signal ready, start pollers, and trigger scoring
		go func() {
			profiler := repoprofile.NewProfiler(ghClient, database, wsHub)
			repos, _ := db.GetConfiguredRepoNames(database)
			if err := profiler.Run(context.Background(), repos, false); err != nil {
				log.Printf("[repoprofile] initial profiling failed: %v", err)
			}
			profileGate.Signal()
			pollerMgr.RestartAll()
			scorer.Trigger()
			bootstrapBareClones(database)
		}()
	} else {
		// Not fully configured — start pollers immediately (may be empty)
		pollerMgr.RestartAll()
	}

	if creds.JiraPAT != "" && creds.JiraURL != "" {
		srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
	}

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
