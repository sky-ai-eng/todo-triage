package postgres_test

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCuratorStore_Postgres_AttributesPerUser pins SKY-298: when Alice
// and Bob each post a message to the same project, the goroutine's
// per-turn SyntheticClaimsWithTx wrap stamps creator_user_id on every
// row that turn produces. We exercise the full write set the goroutine
// would emit (request create + mark running + insert message + insert
// pending_context + complete) for each user separately and verify the
// attribution by reading every row back from the admin pool.
func TestCuratorStore_Postgres_AttributesPerUser(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	bob := seedAdditionalUser(t, h, orgID, "bob")

	// Seed the project via the admin pool — pgstore.New here wires
	// Projects against AppDB (tf_app) and a bare Create call from a
	// test context has no JWT claims set, so the projects_insert RLS
	// policy would reject it. The CuratorStore writes that follow
	// drive each through SyntheticClaimsWithTx with explicit claims,
	// which IS the production goroutine code path.
	projectID := seedPgEntityProject(t, h, orgID, alice, "shared")

	// Alice's turn: capture all the writes the goroutine would do.
	aliceReq, aliceMsg := runFullTurn(t, ctx, stores, orgID, alice, projectID)

	// Bob's turn: same project, different user.
	bobReq, bobMsg := runFullTurn(t, ctx, stores, orgID, bob, projectID)

	// Each request row must attribute to its respective user.
	gotAlice := readCuratorRequestCreator(t, h, aliceReq)
	if gotAlice != alice {
		t.Errorf("alice's request creator_user_id = %s, want %s", gotAlice, alice)
	}
	gotBob := readCuratorRequestCreator(t, h, bobReq)
	if gotBob != bob {
		t.Errorf("bob's request creator_user_id = %s, want %s", gotBob, bob)
	}

	// Curator messages: each row's creator_user_id should match.
	if got := readCuratorMessageCreator(t, h, aliceMsg); got != alice {
		t.Errorf("alice's message creator_user_id = %s, want %s", got, alice)
	}
	if got := readCuratorMessageCreator(t, h, bobMsg); got != bob {
		t.Errorf("bob's message creator_user_id = %s, want %s", got, bob)
	}
}

// TestCuratorStore_Postgres_CrossOrgLeakage pins the defense-in-depth
// org filter: even if Alice in orgA somehow submitted a malformed
// request scoped to orgB's project, the RLS policies + per-statement
// org_id binding would refuse the INSERT. We simulate this by trying
// to call CuratorStore methods under Alice's synthetic claims (orgA)
// against an orgB project id — the writes must not land in orgB.
func TestCuratorStore_Postgres_CrossOrgLeakage(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgA, alice, _ := seedPgProjectOrg(t, h)
	orgB, _, _ := seedPgProjectOrg(t, h)

	// Seed orgA's project via admin — pgstore.New here wires Projects
	// against AppDB (tf_app) and a bare Create call from a test
	// context has no JWT claims set. orgB doesn't need its own
	// project row; the count assertion below just verifies no
	// curator_requests rows land in orgB regardless.
	projectA := seedPgEntityProject(t, h, orgA, alice, "aye")

	// Alice creates a request in orgA — this must work.
	var goodRequestID string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgA, projectA, alice, "hi from alice")
		if err != nil {
			return err
		}
		goodRequestID = id
		return nil
	}); err != nil {
		t.Fatalf("alice valid orgA write: %v", err)
	}
	if goodRequestID == "" {
		t.Fatal("expected non-empty request id from valid write")
	}

	// Cross-org attempt: alice with orgA claims, but binding orgB on
	// the call — the FK against projects(id, org_id) refuses the
	// insert because (projectA, orgB) doesn't exist as a project
	// row; RLS additionally fires on (org_id = current_org_id()).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgA, alice, func(ts db.TxStores) error {
		_, err := ts.Curator.CreateRequest(ctx, orgB, projectA, alice, "cross-org attempt")
		return err
	}); err == nil {
		t.Error("expected cross-org CreateRequest to fail; got nil error")
	}

	// Verify orgB has zero curator_requests rows attributable to Alice.
	var count int
	if err := h.AdminDB.QueryRow(
		`SELECT COUNT(*) FROM curator_requests WHERE org_id = $1 AND creator_user_id = $2`,
		orgB, alice,
	).Scan(&count); err != nil {
		t.Fatalf("count cross-org rows: %v", err)
	}
	if count != 0 {
		t.Errorf("alice has %d rows in orgB after cross-org attempt, want 0", count)
	}
}

// TestCuratorStore_Postgres_GetRequestRLS pins that GetRequest under
// claims for user X cannot read user Y's request row in the same org
// — curator_requests_select RLS gates on creator_user_id =
// tf.current_user_id(). The goroutine's MarkRequestRunning + GetRequest
// pair runs under the requesting user's claims, so this isolation
// matters when the curator runtime grows per-user sessions later
// (SKY-294 / per-user-vs-per-team direction).
func TestCuratorStore_Postgres_GetRequestRLS(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	stores := pgstore.New(h.AdminDB, h.AppDB)
	ctx := context.Background()

	orgID, alice, _ := seedPgProjectOrg(t, h)
	bob := seedAdditionalUser(t, h, orgID, "bob")

	// Admin-seed the project — see comment in AttributesPerUser.
	projectID := seedPgEntityProject(t, h, orgID, alice, "shared")

	// Alice creates a request.
	var aliceReq string
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, alice, "alice's message")
		if err != nil {
			return err
		}
		aliceReq = id
		return nil
	}); err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Bob reads with his own claims — RLS hides alice's row.
	var seenByBob *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, bob, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, orgID, aliceReq)
		if err != nil {
			return err
		}
		seenByBob = r
		return nil
	}); err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if seenByBob != nil {
		t.Errorf("bob's claims should hide alice's request row; got %+v", seenByBob)
	}

	// Alice reads with her own claims — she sees it.
	var seenByAlice *domain.CuratorRequest
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, alice, func(ts db.TxStores) error {
		r, err := ts.Curator.GetRequest(ctx, orgID, aliceReq)
		if err != nil {
			return err
		}
		seenByAlice = r
		return nil
	}); err != nil {
		t.Fatalf("alice get own: %v", err)
	}
	if seenByAlice == nil || seenByAlice.ID != aliceReq {
		t.Errorf("alice should see her own request row; got %+v", seenByAlice)
	}
	if seenByAlice != nil && seenByAlice.CreatorUserID != alice {
		t.Errorf("alice's row CreatorUserID = %s, want %s", seenByAlice.CreatorUserID, alice)
	}
}

// runFullTurn replays the per-message write set the goroutine would
// emit for one turn under (orgID, userID). Returns the request id and
// the inserted message id so the caller can spot-check attribution.
func runFullTurn(t *testing.T, ctx context.Context, stores db.Stores, orgID, userID, projectID string) (requestID string, messageID int64) {
	t.Helper()
	// 1. Create request (handler-side, but in the same identity).
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		id, err := ts.Curator.CreateRequest(ctx, orgID, projectID, userID, "msg from "+userID)
		if err != nil {
			return err
		}
		requestID = id
		return nil
	}); err != nil {
		t.Fatalf("create request for %s: %v", userID, err)
	}
	// 2. Mark running.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		return ts.Curator.MarkRequestRunning(ctx, orgID, requestID)
	}); err != nil {
		t.Fatalf("mark running for %s: %v", userID, err)
	}
	// 3. Insert one streamed message.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		id, err := ts.Curator.InsertMessage(ctx, orgID, &domain.CuratorMessage{
			RequestID: requestID,
			Role:      "assistant",
			Subtype:   "text",
			Content:   "ack for " + userID,
		})
		if err != nil {
			return err
		}
		messageID = id
		return nil
	}); err != nil {
		t.Fatalf("insert message for %s: %v", userID, err)
	}
	// 4. Complete request.
	if err := stores.Tx.SyntheticClaimsWithTx(ctx, orgID, userID, func(ts db.TxStores) error {
		_, err := ts.Curator.CompleteRequest(ctx, orgID, requestID, "done", "", 0.01, 10, 1)
		return err
	}); err != nil {
		t.Fatalf("complete request for %s: %v", userID, err)
	}
	return requestID, messageID
}

func readCuratorRequestCreator(t *testing.T, h *pgtest.Harness, requestID string) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM curator_requests WHERE id = $1`,
		requestID,
	).Scan(&got); err != nil {
		t.Fatalf("read curator_requests.creator_user_id for %s: %v", requestID, err)
	}
	return got
}

func readCuratorMessageCreator(t *testing.T, h *pgtest.Harness, messageID int64) string {
	t.Helper()
	var got string
	if err := h.AdminDB.QueryRow(
		`SELECT creator_user_id::text FROM curator_messages WHERE id = $1`,
		messageID,
	).Scan(&got); err != nil {
		t.Fatalf("read curator_messages.creator_user_id for %d: %v", messageID, err)
	}
	return got
}

// seedAdditionalUser adds a second user to an existing org + the
// org's default team so tests can exercise multi-user attribution
// paths against the same project. Returns the new user's id. The
// team-membership row goes into `memberships`, the same table
// seedPgDefaultTeam writes to — there's no separate
// `team_memberships` table, despite the name pattern elsewhere.
func seedAdditionalUser(t *testing.T, h *pgtest.Harness, orgID, label string) string {
	t.Helper()
	userID := seedPgMember(t, h, orgID, label, "member")
	teamID := firstTeamForOrg(t, h, orgID)
	if _, err := h.AdminDB.Exec(
		`INSERT INTO memberships (user_id, team_id, role) VALUES ($1, $2, 'member')`,
		userID, teamID,
	); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}
	return userID
}
