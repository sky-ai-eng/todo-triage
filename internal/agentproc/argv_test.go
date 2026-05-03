package agentproc

import (
	"slices"
	"testing"
)

func TestBuildArgs_InitialInvocation(t *testing.T) {
	got := BuildArgs(RunOptions{
		Message:      "do the thing",
		Model:        "sonnet-4-6",
		AllowedTools: "Read,Write",
		MaxTurns:     100,
	})
	want := []string{
		"-p", "do the thing",
		"--model", "sonnet-4-6",
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", "Read,Write",
		"--max-turns", "100",
	}
	if !slices.Equal(got, want) {
		t.Errorf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_ResumeAddsResumeFlagBeforeModel(t *testing.T) {
	// --resume must precede --model so claude treats this as a
	// continuation of the captured session, not a fresh run that
	// happens to share a model. Order matters to claude's flag
	// parsing in -p mode.
	got := BuildArgs(RunOptions{
		Message:      "answer to your question",
		SessionID:    "sess-123",
		Model:        "sonnet-4-6",
		AllowedTools: "Read",
	})
	resumeIdx := slices.Index(got, "--resume")
	modelIdx := slices.Index(got, "--model")
	if resumeIdx < 0 || modelIdx < 0 {
		t.Fatalf("missing flags: %v", got)
	}
	if resumeIdx > modelIdx {
		t.Errorf("--resume must come before --model: %v", got)
	}
	if got[resumeIdx+1] != "sess-123" {
		t.Errorf("resume id = %q, want sess-123", got[resumeIdx+1])
	}
}

func TestBuildArgs_OmitsZeroValueFlags(t *testing.T) {
	got := BuildArgs(RunOptions{Message: "hi"})
	for _, flag := range []string{"--model", "--resume", "--allowedTools", "--max-turns"} {
		if slices.Contains(got, flag) {
			t.Errorf("expected %q to be omitted, got %v", flag, got)
		}
	}
	if !slices.Contains(got, "--output-format") || !slices.Contains(got, "--verbose") {
		t.Errorf("missing fixed flags: %v", got)
	}
}
