package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// secretStore is the SQLite impl of db.SecretStore. All three methods
// return ErrNotApplicableInLocal: local-mode credentials live in the
// OS keychain (internal/auth), not the DB. There's no swipes-style
// table to fall back on — this is the first store that's genuinely
// multi-only by design.
//
// The auth.* code path stays the production caller for local-mode
// credentials; SecretStore exists so multi-mode handlers can
// continue to depend on a single interface that just happens to
// fail-closed in local mode if someone wires it wrong.
type secretStore struct{}

func newSecretStore() db.SecretStore { return &secretStore{} }

var _ db.SecretStore = (*secretStore)(nil)

func (*secretStore) Put(ctx context.Context, orgID, key, value, description string) error {
	return db.ErrNotApplicableInLocal
}

func (*secretStore) Get(ctx context.Context, orgID, key string) (string, error) {
	return "", db.ErrNotApplicableInLocal
}

func (*secretStore) Delete(ctx context.Context, orgID, key string) (bool, error) {
	return false, db.ErrNotApplicableInLocal
}
