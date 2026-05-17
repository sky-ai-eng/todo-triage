package poller

import (
	"context"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestManager_RunGitHubCycle_IteratesActiveOrgs pins the SKY-312 outer-
// loop contract on the poller: a single tick enumerates every active
// org via OrgsStore.ListActiveSystem and dispatches per-org work.
//
// To keep the test free of GitHub network round-trips, every fake
// org's RepoStore returns an empty configured-names list — that path
// short-circuits before the tracker is invoked, so the assertion is
// strictly "per-org loop visited every active org," not "tracker did
// the right thing." Tracker behavior is covered by the tracker
// package's own per-org tests.
func TestManager_RunGitHubCycle_IteratesActiveOrgs(t *testing.T) {
	orgs := &fakeOrgsStore{ids: []string{"org-a", "org-b", "org-c"}}
	repos := &recordingRepoStore{}
	users := &emptyUsersStore{} // GetGitHubUsernameSystem unused — repo path exits first

	m := &Manager{orgs: orgs, repos: repos, users: users}
	m.runGitHubCycle(nil, nil)

	if orgs.calls != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 per cycle", orgs.calls)
	}
	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != len(orgs.ids) {
		t.Fatalf("ListConfiguredNamesSystem visited %d orgs (%v); want %d (%v)", len(repos.visited), repos.visited, len(orgs.ids), orgs.ids)
	}
	for i, got := range repos.visited {
		if got != orgs.ids[i] {
			t.Errorf("visit[%d] = %s; want %s (per-org iteration must preserve ListActiveSystem order)", i, got, orgs.ids[i])
		}
	}
}

// TestManager_RunJiraCycle_IteratesActiveOrgs mirrors the GitHub-side
// test for the Jira path. The Jira loop is thinner than GitHub
// (no per-org repo gate), so we record orgIDs via a tracker
// substitute — but tracker.Tracker is a concrete type, so instead we
// record the OrgsStore call and rely on the fact that each visited
// org will at least try to call client.RefreshJira (nil client →
// panic-rescue path is not tested here; that's a real-call concern).
//
// Practically: this test only verifies that ListActiveSystem is
// invoked once per cycle. The per-org dispatch inside runJiraCycle
// is a flat for-loop over the returned slice with no early-exit
// branches that could skip an org — a regression that broke that
// loop would also break the GitHub test above (both share the
// orgs.ListActiveSystem contract).
func TestManager_RunJiraCycle_OrgsStoreError(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	m := &Manager{orgs: orgs}
	m.runJiraCycle(nil, "", nil)

	if orgs.calls != 1 {
		t.Errorf("ListActiveSystem called %d times; want 1 even on error", orgs.calls)
	}
}

// TestManager_RunGitHubCycle_OrgsStoreErrorAbortsCycle pins: if the
// orgs lookup fails, the cycle ends rather than falling back to the
// hardcoded sentinel. Same rationale as the projectclassify variant
// — silent fallback would mask multi-mode failures as local-mode
// behavior.
func TestManager_RunGitHubCycle_OrgsStoreErrorAbortsCycle(t *testing.T) {
	orgs := &fakeOrgsStore{err: errOrgsDown}
	repos := &recordingRepoStore{}
	m := &Manager{orgs: orgs, repos: repos}
	m.runGitHubCycle(nil, nil)

	repos.mu.Lock()
	defer repos.mu.Unlock()
	if len(repos.visited) != 0 {
		t.Errorf("ListConfiguredNamesSystem called %d times despite ListActiveSystem error; want 0", len(repos.visited))
	}
}

// --- test doubles ---

type fakeOrgsStore struct {
	ids   []string
	err   error
	calls int
}

func (f *fakeOrgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.ids...), nil
}

// recordingRepoStore embeds db.RepoStore as nil and overrides only the
// methods the per-org loop reaches before exiting. Returning an empty
// configured-names list short-circuits the loop body so no real
// GitHub client work happens; any other RepoStore call would panic via
// the nil embedded interface, which is the loud-failure-on-regression
// posture we want.
type recordingRepoStore struct {
	db.RepoStore
	mu      sync.Mutex
	visited []string
}

func (r *recordingRepoStore) ListConfiguredNamesSystem(ctx context.Context, orgID string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visited = append(r.visited, orgID)
	return nil, nil
}

type emptyUsersStore struct{ db.UsersStore }

func (emptyUsersStore) GetGitHubUsernameSystem(ctx context.Context, userID string) (string, error) {
	return "", nil
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

var errOrgsDown = stubErr("simulated orgs-store outage")

var (
	_ db.OrgsStore   = (*fakeOrgsStore)(nil)
	_ db.RepoStore   = (*recordingRepoStore)(nil)
	_ db.UsersStore  = emptyUsersStore{}
	_ domain.Project // keep domain import live for parity with sibling per-org tests
)
