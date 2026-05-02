package domain

// Yield types are the three shapes an agent can use to pause a run for
// user input. SKY-139 — see internal/ai/prompts/envelope.txt for the
// agent-facing contract. The frontend renders one of three components
// based on YieldRequest.Type; the backend uses the same discriminator
// to validate that a YieldResponse matches the request.
const (
	YieldTypeConfirmation = "confirmation"
	YieldTypeChoice       = "choice"
	YieldTypePrompt       = "prompt"
)

// YieldRequest is the structured payload the agent emits as the `yield`
// field of its terminal envelope when status == "yield". Stored as the
// content of a run_messages row with subtype == "yield_request" so the
// transcript and the "current open yield" share one source of truth.
//
// Fields are a union — only the ones relevant to Type are populated.
// JSON tags use omitempty so an unmarshalled-then-remarshalled payload
// stays compact, and missing fields don't bleed into the response shape.
type YieldRequest struct {
	Type    string `json:"type"`
	Message string `json:"message"`

	// Confirmation
	AcceptLabel string `json:"accept_label,omitempty"`
	RejectLabel string `json:"reject_label,omitempty"`

	// Choice
	Options []YieldChoiceOption `json:"options,omitempty"`
	Multi   bool                `json:"multi,omitempty"`

	// Prompt
	Placeholder string `json:"placeholder,omitempty"`
}

// YieldChoiceOption is a single item in a Choice yield's options list.
type YieldChoiceOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// YieldResponse is the payload the user submits via POST /api/runs/{id}/respond.
// Same union pattern as YieldRequest — Type echoes the request's Type and the
// per-type field carries the answer.
type YieldResponse struct {
	Type string `json:"type"`

	// Confirmation
	Accepted bool `json:"accepted,omitempty"`

	// Choice — IDs of selected options. Length 1 for single-select; 0+ for multi.
	Selected []string `json:"selected,omitempty"`

	// Prompt — user-entered text.
	Value string `json:"value,omitempty"`
}

// RenderYieldResponseForAgent returns the plain-text string sent back
// to the resumed Claude session. We send natural language rather than
// the raw JSON response — Claude reads "the user accepted" better
// than `{"accepted": true}`. The format is deterministic so the agent
// can match against it programmatically if it wants to.
func RenderYieldResponseForAgent(req *YieldRequest, resp *YieldResponse) string {
	if req == nil || resp == nil {
		return ""
	}
	switch resp.Type {
	case YieldTypeConfirmation:
		if resp.Accepted {
			return "[user response] You asked: " + req.Message + "\nThe user accepted."
		}
		return "[user response] You asked: " + req.Message + "\nThe user declined."
	case YieldTypeChoice:
		labels := labelsForSelected(req, resp.Selected)
		if len(labels) == 0 {
			return "[user response] You asked: " + req.Message + "\nThe user did not select any option."
		}
		joined := joinComma(labels)
		ids := joinComma(resp.Selected)
		return "[user response] You asked: " + req.Message + "\nThe user selected: " + joined + " (option_id" + plural(len(resp.Selected)) + ": " + ids + ")"
	case YieldTypePrompt:
		v := resp.Value
		if v == "" {
			return "[user response] You asked: " + req.Message + "\nThe user submitted an empty response."
		}
		return "[user response] You asked: " + req.Message + "\nThe user replied:\n" + v
	}
	return ""
}

// RenderYieldResponseForDisplay returns the human-readable answer
// shown in the run transcript ("Approved", "Selected: Rebase onto
// main", or the raw user text for prompt yields). Stored as the
// content of the yield_response row so the frontend can render Q+A
// pairs without re-deriving the answer from the structured payload.
func RenderYieldResponseForDisplay(req *YieldRequest, resp *YieldResponse) string {
	if req == nil || resp == nil {
		return ""
	}
	switch resp.Type {
	case YieldTypeConfirmation:
		if resp.Accepted {
			if req.AcceptLabel != "" {
				return req.AcceptLabel
			}
			return "Approved"
		}
		if req.RejectLabel != "" {
			return req.RejectLabel
		}
		return "Declined"
	case YieldTypeChoice:
		labels := labelsForSelected(req, resp.Selected)
		if len(labels) == 0 {
			return "(no selection)"
		}
		return joinComma(labels)
	case YieldTypePrompt:
		return resp.Value
	}
	return ""
}

func labelsForSelected(req *YieldRequest, selected []string) []string {
	if len(selected) == 0 || len(req.Options) == 0 {
		return nil
	}
	byID := make(map[string]string, len(req.Options))
	for _, o := range req.Options {
		byID[o.ID] = o.Label
	}
	out := make([]string, 0, len(selected))
	for _, id := range selected {
		if label, ok := byID[id]; ok && label != "" {
			out = append(out, label)
		} else {
			out = append(out, id)
		}
	}
	return out
}

func joinComma(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
