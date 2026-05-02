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
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/install"
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

	database, err := db.Open()
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
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

	srv := server.New(database)

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

	// Seed event type catalog, task_rules defaults, and default prompts.
	// Order matters: task_rules FK to events_catalog(id), so catalog must be
	// seeded first.
	if err := db.SeedEventTypes(database); err != nil {
		log.Fatalf("failed to seed event types: %v", err)
	}
	if err := db.SeedTaskRules(database); err != nil {
		log.Fatalf("failed to seed task rules: %v", err)
	}
	seedDefaultPrompts(database)

	// Auto-import Claude Code skill files as prompts
	skills.ImportAll(database)

	// Event bus — central pub/sub replacing direct callbacks
	bus := eventbus.New()

	wsHub := srv.WSHub()

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

	scorer := ai.NewRunner(database, ai.RunnerCallbacks{
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

	// Poller manager — uses event bus instead of direct callbacks.
	// Poll errors are toasted with per-source time-based throttling: the
	// poller fires OnError on every failure (raw signal), but we only
	// refresh the user-facing toast every errorToastMinInterval. Without
	// throttling, a persistent failure (expired PAT, network outage) would
	// generate a sticky error toast every poll cycle (default 60s) until
	// the user manually dismissed each one — badly spammy on the UI.
	const errorToastMinInterval = 5 * time.Minute
	var (
		errorThrottleMu sync.Mutex
		lastErrorToast  = map[string]time.Time{}
	)
	pollerMgr := poller.NewManager(database, bus)
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
	spawner := delegate.NewSpawner(database, nil, wsHub, "")
	srv.SetSpawner(spawner)

	// Event router — records events, creates/bumps tasks, auto-delegates on
	// matching triggers, runs inline close checks. Also handles post-scoring
	// re-derive via the scorer callback wired above.
	eventRouter = routing.NewRouter(database, spawner, scorer, wsHub)
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

		profileGate.Invalidate()
		pollerMgr.StopAll()

		cfg, _ := config.Load()
		creds, _ := auth.Load()

		if cfg.GitHub.Ready(creds.GitHubPAT, creds.GitHubURL) {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner.UpdateCredentials(ghClient, cfg.AI.Model)
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
			srv.SetGitHubClient(nil)
			pollerMgr.RestartAll()
		}

		// Also refresh Jira client in case it's configured
		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
		} else {
			srv.SetJiraClient(nil, config.JiraStatusRule{})
		}
	})

	// Jira changed: restart only the Jira poller
	srv.SetOnJiraChanged(func() {
		log.Println("[server] Jira config changed, restarting Jira poller...")
		setAnnouncePending("jira")

		cfg, _ := config.Load()
		creds, _ := auth.Load()

		pollerMgr.RestartJira()

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
		} else {
			srv.SetJiraClient(nil, config.JiraStatusRule{})
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
		srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT), cfg.Jira.InProgress)
	}

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
