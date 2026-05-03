package domain

import "time"

// CuratorRequest is one user→agent exchange with a project's Curator.
// One row per posted message: status flips from queued → running →
// terminal; cost / duration / num_turns are stamped at termination
// (mirrors what AgentRun does for delegated runs).
//
// The user's own input lives on UserInput here rather than as a row
// in curator_messages — that table holds only the agent's side of the
// exchange, same way run_messages does.
type CuratorRequest struct {
	ID         string
	ProjectID  string
	Status     string // queued | running | done | cancelled | failed
	UserInput  string
	ErrorMsg   string
	CostUSD    float64
	DurationMs int
	NumTurns   int
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// IsTerminal reports whether the request has reached a final status.
// Used by the cancel endpoint to 404 when the in-flight slot is empty
// and by the per-project goroutine to skip already-finalized rows.
func (r *CuratorRequest) IsTerminal() bool {
	switch r.Status {
	case "done", "cancelled", "failed":
		return true
	}
	return false
}

// CuratorMessage mirrors run_messages but is keyed by request_id
// instead of run_id. The on-the-wire shape is otherwise identical so
// the frontend's existing message-rendering can be reused without
// branching on which table the row came from.
type CuratorMessage struct {
	ID                  int
	RequestID           string
	Role                string
	Subtype             string
	Content             string
	ToolCalls           []ToolCall
	ToolCallID          string
	IsError             bool
	Metadata            map[string]any
	Model               string
	InputTokens         *int
	OutputTokens        *int
	CacheReadTokens     *int
	CacheCreationTokens *int
	CreatedAt           time.Time
}
