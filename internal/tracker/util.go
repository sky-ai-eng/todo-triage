package tracker

import (
	"encoding/json"
	"fmt"
	"log"
)

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[tracker] mustJSON marshal error: %v", err)
		return "{}"
	}
	return string(data)
}

// ghSourceID returns a globally unique source_id for a GitHub PR.
// PR numbers are only unique within a repo, so we prefix with "owner/repo#".
func ghSourceID(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// scopedQueries takes a base search query and returns one or more queries
// with " repo:owner/name" qualifiers appended, batched to stay under maxSearchQueryLen.
// If no repos are configured, returns the base query as-is.
func scopedQueries(base string, repos []string) []string {
	if len(repos) == 0 {
		return []string{base}
	}

	var queries []string
	current := base
	for _, repo := range repos {
		term := " repo:" + repo
		if len(current)+len(term) > maxSearchQueryLen {
			queries = append(queries, current)
			current = base + term
		} else {
			current += term
		}
	}
	queries = append(queries, current)
	return queries
}
