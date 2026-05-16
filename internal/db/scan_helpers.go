package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// rowScanner is the common Scan surface of *sql.Row and *sql.Rows.
// Used by package-level legacy curator helpers (curator.go +
// curator_pending_context.go) that still run raw SQL inside their
// own transactions, predating the store migration. Same shape as
// the sqlite-package helper of the same name.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanProject is a SQLite-shape row scanner used only by
// internal/db/curator_pending_context.go to read a project inside a
// curator-specific tx that doesn't go through Stores.Tx.WithTx. The
// store-level scanners live in internal/db/{sqlite,postgres}/projects.go
// — when CuratorStore lands and curator_pending_context migrates,
// this helper goes away.
func scanProject(row rowScanner) (*domain.Project, error) {
	var (
		p            domain.Project
		sessionID    sql.NullString
		jiraKey      sql.NullString
		linearKey    sql.NullString
		specPromptID sql.NullString
		pinnedJSON   string
		createdAt    time.Time
		updatedAt    time.Time
	)
	err := row.Scan(&p.ID, &p.Name, &p.Description, &sessionID, &pinnedJSON, &jiraKey, &linearKey, &specPromptID, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CuratorSessionID = sessionID.String
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipPromptID = specPromptID.String
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if pinnedJSON == "" {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal([]byte(pinnedJSON), &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
