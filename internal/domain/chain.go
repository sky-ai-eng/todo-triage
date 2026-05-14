package domain

import "time"

// ChainRunStatus is the lifecycle state of a ChainRun.
type ChainRunStatus string

// ChainRun statuses. running until any terminal; aborted when a step
// records --abort or omits a verdict; failed for infrastructure errors;
// cancelled when the user cancels mid-chain.
const (
	ChainRunStatusRunning   ChainRunStatus = "running"
	ChainRunStatusCompleted ChainRunStatus = "completed"
	ChainRunStatusAborted   ChainRunStatus = "aborted"
	ChainRunStatusFailed    ChainRunStatus = "failed"
	ChainRunStatusCancelled ChainRunStatus = "cancelled"
)

// ChainTriggerType distinguishes how a chain was initiated.
type ChainTriggerType string

const (
	ChainTriggerManual ChainTriggerType = "manual"
	ChainTriggerEvent  ChainTriggerType = "event"
)

// ChainStep is one position in a chain prompt's ordered step list.
// step_index is 0-based and densely packed by ReplaceChainSteps.
type ChainStep struct {
	ChainPromptID string    `json:"chain_prompt_id"`
	StepIndex     int       `json:"step_index"`
	StepPromptID  string    `json:"step_prompt_id"`
	Brief         string    `json:"brief"`
	CreatedAt     time.Time `json:"created_at"`
}

// ChainRun is the chain instance. One row per Delegate(chainPrompt, ...)
// call. Owns the shared worktree across all steps. Per-step state
// lives on the runs table linked back via runs.chain_run_id.
type ChainRun struct {
	ID            string           `json:"id"`
	ChainPromptID string           `json:"chain_prompt_id"`
	TaskID        string           `json:"task_id"`
	TriggerType   ChainTriggerType `json:"trigger_type"`
	TriggerID     string           `json:"trigger_id,omitempty"`
	Status        ChainRunStatus   `json:"status"`
	AbortReason   string           `json:"abort_reason,omitempty"`
	AbortedAtStep *int             `json:"aborted_at_step,omitempty"`
	WorktreePath  string           `json:"worktree_path"`
	StartedAt     time.Time        `json:"started_at"`
	CompletedAt   *time.Time       `json:"completed_at,omitempty"`
}

// ChainVerdictOutcome is the tri-state result of a chain step's verdict.
type ChainVerdictOutcome string

const (
	ChainVerdictAdvance ChainVerdictOutcome = "advance"
	ChainVerdictAbort   ChainVerdictOutcome = "abort"
	ChainVerdictFinal   ChainVerdictOutcome = "final"
)

// ChainVerdict is the structured handoff a chain step records via
// `triagefactory exec chain verdict`. Stored as run_artifacts.metadata_json
// with kind='chain:verdict'. Latest by created_at wins per step (idempotent
// re-recording within a step).
//
// Outcome semantics (replaces old Proceed/Final bool pair):
//   - ChainVerdictAdvance → advance to next step  (was Proceed=true,  Final=false)
//   - ChainVerdictAbort   → abort the chain; leave task open for human (was Proceed=false, Final=false)
//   - ChainVerdictFinal   → end the chain successfully at this step; close the task (was Proceed=false, Final=true)
//
// The old Proceed=true, Final=true combination was invalid and is now unrepresentable.
type ChainVerdict struct {
	Outcome   ChainVerdictOutcome `json:"outcome"`
	Reason    string              `json:"reason"`
	Notes     string              `json:"notes,omitempty"`
	Synthetic bool                `json:"-"` // internal flag, never on wire
}
