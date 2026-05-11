package ai

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunnerCallbacks are optional hooks fired during the scoring lifecycle.
// The caller wires these to WS broadcasts or other side effects.
type RunnerCallbacks struct {
	OnScoringStarted   func(taskIDs []string)
	OnScoringCompleted func(taskIDs []string)
	// OnTasksSkipped fires once per scoring cycle if one or more batches
	// errored. skipped is the exact count of tasks that weren't scored;
	// total is len(tasks) at cycle start. Wired to a warning toast in main
	// so the user knows tasks were skipped without log-diving. Fatal errors
	// (DB failures) go through OnError.
	OnTasksSkipped func(skipped, total int)
	// OnError fires on fatal scoring errors (query, write, or scorer-returned
	// errors that abort the cycle).
	OnError func(err error)
}

// Runner manages AI scoring as a background process.
// It exposes a Trigger channel that pollers signal after ingesting new tasks.
type Runner struct {
	database     *sql.DB
	callbacks    RunnerCallbacks
	profileReady func() bool // returns true when repo profiles are available
	trigger      chan struct{}
	stop         chan struct{}
	done         chan struct{} // closed when the run loop exits
	mu           sync.Mutex
	running      bool
}

func NewRunner(database *sql.DB, callbacks RunnerCallbacks) *Runner {
	return &Runner{
		database:  database,
		callbacks: callbacks,
		trigger:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// SetProfileGate sets the function used to check if repo profiles are ready.
// If not set, scoring proceeds without gating. Must be called before Start()
// or protected by the mutex for concurrent access.
func (r *Runner) SetProfileGate(fn func() bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profileReady = fn
}

// Trigger signals the runner to check for unscored tasks.
// Non-blocking — if a scoring run is already pending, the signal is merged.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
		// already triggered, skip
	}
}

// reportError invokes the OnError callback if set.
func (r *Runner) reportError(err error) {
	if r.callbacks.OnError != nil {
		r.callbacks.OnError(err)
	}
}

func (r *Runner) Start() {
	// Derive a ctx that cancels when Stop() closes r.stop. This ctx is
	// passed into each run() so any in-flight scoring agent (which now
	// goes through agentproc.Run → SDK subprocess) gets SIGKILL'd on
	// server shutdown rather than blocking the shutdown until the model
	// times out on its own.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-r.stop
		cancel()
	}()
	go func() {
		defer close(r.done)
		for {
			select {
			case <-r.trigger:
				r.run(ctx)
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop signals the runner to stop and waits for the run loop to exit.
func (r *Runner) Stop() {
	close(r.stop)
	<-r.done
}

func (r *Runner) run(ctx context.Context) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	// Wait for repo profiles before scoring — stale or missing profiles
	// lead to incorrect repo matches that would need re-scoring anyway.
	r.mu.Lock()
	gate := r.profileReady
	r.mu.Unlock()
	if gate != nil && !gate() {
		log.Println("[ai] skipping scoring cycle: repo profiles not ready")
		return
	}

	tasks, err := db.UnscoredTasks(r.database)
	if err != nil {
		log.Printf("[ai] error fetching unscored tasks: %v", err)
		r.reportError(err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	log.Printf("[ai] scoring %d unscored tasks...", len(tasks))

	// Collect task IDs for callbacks
	taskIDs := make([]string, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}

	// Persist scoring state before calling AI
	if err := db.MarkScoring(r.database, taskIDs); err != nil {
		log.Printf("[ai] error marking tasks as scoring: %v", err)
	}

	if r.callbacks.OnScoringStarted != nil {
		r.callbacks.OnScoringStarted(taskIDs)
	}

	scores, skippedTasks, err := ScoreTasks(ctx, r.database, tasks)
	if err != nil {
		log.Printf("[ai] scoring failed: %v", err)
		r.reportError(err)
		// Fatal scoring error — every task was MarkScoring'd but none of
		// them will be transitioned to 'scored'. Reset the whole set back
		// to 'pending' so the next cycle retries them; otherwise they stay
		// stuck forever (UnscoredTasks only picks 'pending').
		if resetErr := db.ResetScoringToPending(r.database, taskIDs); resetErr != nil {
			log.Printf("[ai] warning: failed to reset tasks to pending after scoring failure: %v", resetErr)
		}
		return
	}

	// Reset tasks that were in failed batches back to 'pending' so they
	// retry next cycle. Without this, a per-batch failure leaves those
	// tasks marked 'in_progress' forever since UpdateTaskScores only
	// transitions successfully-scored ones to 'scored'.
	if skippedTasks > 0 {
		scoredIDs := make(map[string]struct{}, len(scores))
		for _, s := range scores {
			scoredIDs[s.ID] = struct{}{}
		}
		var skippedIDs []string
		for _, id := range taskIDs {
			if _, ok := scoredIDs[id]; !ok {
				skippedIDs = append(skippedIDs, id)
			}
		}
		if len(skippedIDs) > 0 {
			if resetErr := db.ResetScoringToPending(r.database, skippedIDs); resetErr != nil {
				log.Printf("[ai] warning: failed to reset %d skipped tasks to pending: %v", len(skippedIDs), resetErr)
			}
		}
		if r.callbacks.OnTasksSkipped != nil {
			r.callbacks.OnTasksSkipped(skippedTasks, len(tasks))
		}
	}

	updates := make([]domain.TaskScoreUpdate, len(scores))
	for i, s := range scores {
		updates[i] = domain.TaskScoreUpdate{
			ID:                  s.ID,
			PriorityScore:       s.PriorityScore,
			AutonomySuitability: s.AutonomySuitability,
			PriorityReasoning:   s.PriorityReasoning,
			Summary:             s.Summary,
		}
	}

	if err := db.UpdateTaskScores(r.database, updates); err != nil {
		log.Printf("[ai] error saving scores: %v", err)
		r.reportError(err)
		// UpdateTaskScores failing means the in-memory scores are lost AND
		// the scored tasks are still marked 'in_progress'. Reset everything
		// still in that state so the next cycle re-scores. Previously-reset
		// skipped tasks are already 'pending' and the reset is idempotent.
		if resetErr := db.ResetScoringToPending(r.database, taskIDs); resetErr != nil {
			log.Printf("[ai] warning: failed to reset tasks to pending after save failure: %v", resetErr)
		}
		return
	}

	log.Printf("[ai] scored %d tasks successfully", len(updates))

	if r.callbacks.OnScoringCompleted != nil {
		r.callbacks.OnScoringCompleted(taskIDs)
	}
}
