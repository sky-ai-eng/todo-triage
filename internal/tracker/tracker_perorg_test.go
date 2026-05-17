package tracker

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// TestRefreshJira_PropagatesOrgIDToEntityStore pins the SKY-312
// function-parameter contract: RefreshJira no longer hardcodes the
// runmode sentinel — every entity-store read/write is bound to the
// orgID passed by the caller (the poller manager's per-org loop).
//
// The test uses the empty-projects fast path (discoverJira returns nil
// for len(projects)==0) so no Jira client call is required. The first
// reachable entity-store call after discovery is ListActiveSystem,
// which lets the test capture the orgID without setting up snapshot
// diff scaffolding.
func TestRefreshJira_PropagatesOrgIDToEntityStore(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	entities := &recordingEntityStore{}
	tr := &Tracker{bus: bus, entities: entities}

	const wantOrg = "00000000-0000-0000-0000-000000000abc"
	if _, err := tr.RefreshJira(wantOrg, nil, "", nil); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	entities.mu.Lock()
	defer entities.mu.Unlock()
	if len(entities.listActiveOrgIDs) == 0 {
		t.Fatal("ListActiveSystem was not called; the tracker should query entities for the target org")
	}
	for i, got := range entities.listActiveOrgIDs {
		if got != wantOrg {
			t.Errorf("ListActiveSystem[%d] received orgID %q; want %q (SKY-312 expects per-call orgID propagation)", i, got, wantOrg)
		}
	}
}

// --- test doubles ---

// recordingEntityStore embeds db.EntityStore as nil and overrides
// ListActiveSystem (the only entity-store method reached by the
// empty-projects test path). Any unexpected EntityStore call panics
// via the nil embedded interface — a regression that bypasses the
// fast-path early-exit fails loudly.
type recordingEntityStore struct {
	db.EntityStore
	mu               sync.Mutex
	listActiveOrgIDs []string
}

func (r *recordingEntityStore) ListActiveSystem(ctx context.Context, orgID, source string) ([]domain.Entity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listActiveOrgIDs = append(r.listActiveOrgIDs, orgID)
	return nil, nil
}
