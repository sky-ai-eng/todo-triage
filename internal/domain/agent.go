package domain

import "time"

// AgentRun represents a delegated agent execution.
type AgentRun struct {
	ID            string
	TaskID        string
	Status        string // "cloning" | "fetching" | "worktree_created" | "agent_starting" | "running" | "completed" | "failed" | "cancelled"
	Model         string
	StartedAt     time.Time
	CompletedAt   *time.Time
	TotalCostUSD  *float64
	DurationMs    *int
	NumTurns      *int
	StopReason    string
	WorktreePath  string
	ResultLink    string
	ResultSummary string
}

// AgentMessage represents a single message within an agent run.
type AgentMessage struct {
	ID                  int
	RunID               string
	Role                string // "assistant" | "tool"
	Content             string
	Subtype             string // "text" | "thinking" | "tool_use" | "tool"
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

// ToolCall represents a single tool invocation within a message.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}
