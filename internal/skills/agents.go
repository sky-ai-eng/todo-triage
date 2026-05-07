package skills

import (
	"os"
	"path/filepath"
	"strings"
)

// ScanAgentTools reads all agent definitions from ~/.claude/agents/*.md
// and collects their declared tools. Returns a deduplicated, comma-
// separated string suitable for merging into --allowedTools — or ""
// if no agent files exist or none declare tools.
func ScanAgentTools() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".claude", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	var parts []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		frontmatter, _ := splitFrontmatter(string(data))
		if frontmatter == "" {
			continue
		}
		tools := parseToolsFrontmatter(frontmatter, "tools")
		if tools != "" {
			parts = append(parts, tools)
		}
	}
	return NormalizeToolList(strings.Join(parts, ","))
}
