package agentproc

import (
	"strings"
	"testing"
)

func TestBuildAllowedToolsWithExtras_Empty(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", "")
	if got != base {
		t.Errorf("empty extras should return base unchanged")
	}
}

func TestBuildAllowedToolsWithExtras_AddsMCPTools(t *testing.T) {
	extras := "mcp__acme-docs__search_api,mcp__widget-srv__get_schema"
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", extras)
	if !strings.Contains(got, "mcp__acme-docs__search_api") {
		t.Error("expected mcp__acme-docs__search_api in result")
	}
	if !strings.Contains(got, "mcp__widget-srv__get_schema") {
		t.Error("expected mcp__widget-srv__get_schema in result")
	}
}

func TestBuildAllowedToolsWithExtras_DeduplicatesBaseTools(t *testing.T) {
	extras := "Read,Write,mcp__new_tool"
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", extras)
	// Read and Write are already in base, so only mcp__new_tool should be added
	count := strings.Count(got, "Read")
	if count != 1 {
		t.Errorf("Read appears %d times, want 1", count)
	}
}

func TestBuildAllowedToolsWithExtras_WhitespaceOnly(t *testing.T) {
	base := BuildAllowedTools("/usr/local/bin/tf")
	got := BuildAllowedToolsWithExtras("/usr/local/bin/tf", "   ")
	if got != base {
		t.Errorf("whitespace-only extras should return base unchanged")
	}
}
