package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// orgsStore is the SQLite impl of db.OrgsStore. The local-mode orgs
// table has no soft-delete column — every row is considered active.
// In practice this returns the single runmode.LocalDefaultOrgID
// sentinel seeded by the v1.11.0 baseline migration, but the SQL
// makes no assumption about that count so a hypothetical future test
// fixture that inserts additional rows iterates them correctly.
type orgsStore struct{ q queryer }

func newOrgsStore(q queryer) db.OrgsStore { return &orgsStore{q: q} }

var _ db.OrgsStore = (*orgsStore)(nil)

func (s *orgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id FROM orgs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
