package projectclassify

import (
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Runner manages the project-classification background loop. Mirrors
// the shape of internal/ai/Runner: a buffered trigger channel, idempotent
// during an active cycle, started/stopped from main.go. Pollers signal
// `Trigger()` after a poll cycle finishes (via an event-bus subscriber
// in main.go) and the runner picks up any newly-discovered entities
// that haven't been classified yet.
type Runner struct {
	database *sql.DB
	trigger  chan struct{}
	stop     chan struct{}
	mu       sync.Mutex
	running  bool
}

func NewRunner(database *sql.DB) *Runner {
	return &Runner{
		database: database,
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Trigger signals the runner to check for unclassified entities.
// Non-blocking — if a cycle is already pending, the signal is merged.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

func (r *Runner) Start() {
	go func() {
		for {
			select {
			case <-r.trigger:
				r.run()
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *Runner) Stop() {
	close(r.stop)
}

func (r *Runner) run() {
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

	entities, err := db.ListUnclassifiedEntities(r.database)
	if err != nil {
		log.Printf("[classify] list unclassified entities: %v", err)
		return
	}
	if len(entities) == 0 {
		return
	}

	projects, err := db.ListProjects(r.database)
	if err != nil {
		log.Printf("[classify] list projects: %v", err)
		return
	}

	if len(projects) == 0 {
		// No projects to vote — stamp classified_at on every unclassified
		// entity so we don't re-fire on every poll cycle. The
		// project-creation popup is the path to retro-assign these once
		// projects exist.
		for _, e := range entities {
			if err := db.AssignEntityProject(r.database, e.ID, nil, ""); err != nil {
				log.Printf("[classify] stamp classified_at for %s: %v", e.ID, err)
			}
		}
		return
	}

	log.Printf("[classify] classifying %d entities against %d projects", len(entities), len(projects))

	assigned := 0
	for _, e := range entities {
		winner, votes := Classify(projects, e)
		rationale := bestRationale(votes)
		if winner != nil {
			log.Printf("[classify] %s -> project %s (winning vote)", e.ID, *winner)
			assigned++
		} else if len(votes) > 0 {
			best := -1
			for _, v := range votes {
				if v.Err == nil && v.Score > best {
					best = v.Score
				}
			}
			log.Printf("[classify] %s unassigned (best score: %d, threshold: %d)", e.ID, best, ConfidenceThreshold)
		}
		if err := db.AssignEntityProject(r.database, e.ID, winner, rationale); err != nil {
			log.Printf("[classify] assign %s: %v", e.ID, err)
		}
	}
	log.Printf("[classify] cycle complete: %d/%d entities assigned", assigned, len(entities))
}

// bestRationale picks the rationale of the highest-scoring vote (winner
// or runner-up), so unassigned entities still record "closest match was
// X at N/100, because: …". Errored votes are skipped. Returns empty
// string if no successful vote exists.
func bestRationale(votes []Vote) string {
	bestScore := -1
	best := ""
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		if v.Score > bestScore {
			bestScore = v.Score
			best = v.Rationale
		}
	}
	return best
}
