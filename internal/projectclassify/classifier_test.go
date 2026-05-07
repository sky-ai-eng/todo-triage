package projectclassify

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// stubHaiku swaps runHaiku for the duration of a test, restoring the
// real implementation when t.Cleanup fires. The stub takes a function
// keyed on the project name embedded in the prompt so different
// projects can return different scores.
func stubHaiku(t *testing.T, scoresByProjectName map[string]int) *callRecorder {
	t.Helper()
	rec := &callRecorder{}
	orig := runHaiku
	runHaiku = func(prompt string) (int, string, error) {
		rec.record(prompt)
		for name, score := range scoresByProjectName {
			if strings.Contains(prompt, "<project_name>\n"+name+"\n</project_name>") {
				return score, "stub rationale", nil
			}
		}
		return 0, "no stub match", nil
	}
	t.Cleanup(func() { runHaiku = orig })
	return rec
}

type callRecorder struct {
	mu     sync.Mutex
	calls  int
	prompt []string
}

func (c *callRecorder) record(p string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.prompt = append(c.prompt, p)
}

func TestClassify_WinnerAboveThreshold(t *testing.T) {
	stubHaiku(t, map[string]int{
		"Auth Migration": 85,
		"Misc Work":      20,
	})

	projects := []domain.Project{
		{ID: "p-auth", Name: "Auth Migration", Description: "Replace session storage with JWT"},
		{ID: "p-misc", Name: "Misc Work", Description: ""},
	}
	entity := domain.Entity{
		ID:     "e1",
		Source: "github", SourceID: "owner/repo#42",
		Title: "Migrate session token validation",
	}

	winner, votes := Classify(projects, entity)
	if winner == nil {
		t.Fatalf("expected winner, got nil; votes: %+v", votes)
	}
	if *winner != "p-auth" {
		t.Errorf("winner = %s, want p-auth", *winner)
	}
}

func TestClassify_AllBelowThreshold_ReturnsNil(t *testing.T) {
	stubHaiku(t, map[string]int{
		"Misc Work":     20,
		"Other Project": 45,
	})
	projects := []domain.Project{
		{ID: "p1", Name: "Misc Work"},
		{ID: "p2", Name: "Other Project"},
	}
	entity := domain.Entity{ID: "e1", Title: "Random PR"}

	winner, votes := Classify(projects, entity)
	if winner != nil {
		t.Errorf("expected nil winner, got %s", *winner)
	}
	if len(votes) != 2 {
		t.Errorf("expected 2 votes, got %d", len(votes))
	}
}

func TestClassify_HighestAboveThresholdWins(t *testing.T) {
	stubHaiku(t, map[string]int{
		"P1": 65,
		"P2": 90,
		"P3": 70,
	})
	projects := []domain.Project{
		{ID: "p1", Name: "P1"},
		{ID: "p2", Name: "P2"},
		{ID: "p3", Name: "P3"},
	}
	winner, _ := Classify(projects, domain.Entity{Title: "X"})
	if winner == nil || *winner != "p2" {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want p2", got)
	}
}

func TestClassify_TiesGoToFirstReturned(t *testing.T) {
	stubHaiku(t, map[string]int{
		"Alpha": 75,
		"Beta":  75,
	})
	projects := []domain.Project{
		{ID: "p-alpha", Name: "Alpha"},
		{ID: "p-beta", Name: "Beta"},
	}
	winner, _ := Classify(projects, domain.Entity{Title: "X"})
	if winner == nil {
		t.Fatal("expected winner")
	}
	if *winner != "p-alpha" {
		t.Errorf("expected p-alpha (first-returned tie), got %s", *winner)
	}
}

func TestClassify_NoProjects_ReturnsNilNoVotes(t *testing.T) {
	winner, votes := Classify(nil, domain.Entity{Title: "X"})
	if winner != nil {
		t.Errorf("expected nil winner")
	}
	if len(votes) != 0 {
		t.Errorf("expected zero votes, got %d", len(votes))
	}
}

func TestClassify_HaikuErrorTreatedAsNoVote(t *testing.T) {
	orig := runHaiku
	t.Cleanup(func() { runHaiku = orig })
	runHaiku = func(prompt string) (int, string, error) {
		if strings.Contains(prompt, "<project_name>\nFlaky\n</project_name>") {
			return 0, "", errors.New("simulated CLI failure")
		}
		return 80, "ok", nil
	}

	projects := []domain.Project{
		{ID: "p-flaky", Name: "Flaky"},
		{ID: "p-good", Name: "Healthy"},
	}
	winner, votes := Classify(projects, domain.Entity{Title: "X"})
	if winner == nil || *winner != "p-good" {
		got := "<nil>"
		if winner != nil {
			got = *winner
		}
		t.Errorf("winner = %s, want p-good", got)
	}
	for _, v := range votes {
		if v.ProjectID == "p-flaky" && v.Err == nil {
			t.Errorf("flaky vote should carry Err")
		}
	}
}

// TestClassifyPrompt_IncludesCalibrationLanguage is a regression guard
// against accidentally weakening the prompt's "score LOW when uncertain"
// posture. The exact phrase is what makes "always vote, even on
// thin-context projects" safe — if it goes missing, name-only projects
// could over-claim entities. SKY-220.
func TestClassifyPrompt_IncludesCalibrationLanguage(t *testing.T) {
	must := []string{
		"Lack of information is a reason to score LOW",
		"return a score below 30",
		"Do NOT round up",
	}
	// Render the prompt template against an empty project so we assert
	// against the literal template content, not user data.
	p := fmt.Sprintf(classifyPrompt, "", "", "", "", "", "", "", "")
	for _, snippet := range must {
		if !strings.Contains(p, snippet) {
			t.Errorf("classify prompt missing calibration phrase %q", snippet)
		}
	}
}
