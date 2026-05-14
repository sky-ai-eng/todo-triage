package domain

import "time"

// PromptKind distinguishes single-prompt leaves from multi-step chains.
type PromptKind string

const (
	PromptKindLeaf  PromptKind = "leaf"
	PromptKindChain PromptKind = "chain"
)

// Prompt is a user- or system-defined delegation prompt template.
// The body contains the "mission" — what the agent should do.
// The system envelope (tool guidance, completion format, repo scoping) is always
// injected by the spawner and is not part of the prompt body.
type Prompt struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Body         string     `json:"body"`
	Source       string     `json:"source"`        // "system", "user", "imported"
	Kind         PromptKind `json:"kind"`          // PromptKindLeaf | PromptKindChain (defaults to "leaf")
	AllowedTools string     `json:"allowed_tools"` // comma-separated extra tools parsed from SKILL.md/agent frontmatter
	Model        string     `json:"model"`         // per-prompt model override; "" = inherit settings.AI.Model at dispatch
	UsageCount   int        `json:"usage_count"`   // how many agent runs have used this prompt
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
