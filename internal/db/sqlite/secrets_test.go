package sqlite_test

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSecretStore_SQLite_AllMethodsReturnNotApplicable pins the
// contract for the local-mode SecretStore: every method returns
// db.ErrNotApplicableInLocal, never a panic and never a silent
// success. Local-mode credentials live in the OS keychain
// (internal/auth); SecretStore is multi-only by design.
func TestSecretStore_SQLite_AllMethodsReturnNotApplicable(t *testing.T) {
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.Secrets.Put(ctx, runmode.LocalDefaultOrg, "k", "v", ""); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Fatalf("Put err=%v want ErrNotApplicableInLocal", err)
	}
	if _, err := stores.Secrets.Get(ctx, runmode.LocalDefaultOrg, "k"); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Fatalf("Get err=%v want ErrNotApplicableInLocal", err)
	}
	if _, err := stores.Secrets.Delete(ctx, runmode.LocalDefaultOrg, "k"); !errors.Is(err, db.ErrNotApplicableInLocal) {
		t.Fatalf("Delete err=%v want ErrNotApplicableInLocal", err)
	}
}
