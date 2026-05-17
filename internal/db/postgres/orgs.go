package postgres

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// orgsStore is the Postgres impl of db.OrgsStore. Every method routes
// through the admin pool — see the OrgsStore interface comment for the
// pool-split rationale.
type orgsStore struct{ admin queryer }

func newOrgsStore(admin queryer) db.OrgsStore { return &orgsStore{admin: admin} }

var _ db.OrgsStore = (*orgsStore)(nil)

func (s *orgsStore) ListActiveSystem(ctx context.Context) ([]string, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT id FROM orgs
		WHERE deleted_at IS NULL
		ORDER BY id ASC
	`)
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
