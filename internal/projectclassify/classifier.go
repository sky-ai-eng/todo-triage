// Package projectclassify decides which project (if any) a newly-
// discovered entity belongs to. SKY-220.
//
// One Haiku call per project — each project votes independently with its
// own context (name + description + concatenated knowledge-base) and
// returns a 0-100 confidence score. The highest score above
// ConfidenceThreshold wins. All-below-threshold leaves the entity
// unassigned (project_id stays NULL) and the entity resurfaces in the
// next project-creation backfill popup.
//
// Single-entity-per-call rather than batched. Discoveries are rare
// (a few per poll cycle at most) and each call already inlines the
// full per-project context, so batching across entities would just
// duplicate context across the wire.
package projectclassify

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:embed prompts/classify.txt
var classifyPrompt string

// ConfidenceThreshold is the minimum Haiku score (0-100) required to
// auto-assign an entity to a project. Below this, project_id stays
// NULL and the entity resurfaces in future project-creation backfill
// popups. 60 is a launch default; tune from real votes.
const ConfidenceThreshold = 60

// classifyModel mirrors the scorer's model choice — fast and cheap, and
// we want consistent Haiku-version drift across the two AI surfaces.
const classifyModel = "haiku"

// maxConcurrentVotes caps the number of in-flight Haiku calls per
// classify cycle. Most installs have <10 projects so the cap rarely
// fires; the bound exists to avoid swamping the local `claude` CLI on
// pathological setups.
const maxConcurrentVotes = 8

// entityDescriptionMaxLen mirrors the scorer's truncation policy
// (internal/ai/scorer.go:descriptionMaxLen). The classifier prompt
// already includes title; the description is supplementary context
// that doesn't need to be unbounded.
const entityDescriptionMaxLen = 1500

// kbInlineMaxBytes caps the per-project knowledge-base content sent to
// each Haiku call. Curator-written KBs are typically a handful of KB;
// the cap exists for the pathological case where a user dumps a large
// reference document. Above this we truncate with a sentinel rather
// than failing — partial context still beats no vote.
const kbInlineMaxBytes = 50 * 1024

// Vote is the per-project result of one classification call. Exposed
// for tests + diagnostic logging; production code only consumes the
// winner via Classify.
type Vote struct {
	ProjectID string
	Score     int
	Rationale string
	Err       error
}

// Classify runs the per-project quorum vote for one entity. Returns the
// winning project_id (or nil if all votes are below threshold) plus the
// per-project vote slice for logging. Errors loading project context
// are logged and the project is treated as a no-vote rather than
// aborting the whole classification.
func Classify(projects []domain.Project, entity domain.Entity) (*string, []Vote) {
	if len(projects) == 0 {
		return nil, nil
	}

	sem := make(chan struct{}, maxConcurrentVotes)
	votes := make([]Vote, len(projects))
	var wg sync.WaitGroup

	for i, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, project domain.Project) {
			defer wg.Done()
			defer func() { <-sem }()
			votes[idx] = voteFor(project, entity)
		}(i, p)
	}
	wg.Wait()

	winner := pickWinner(votes)
	if winner == nil {
		return nil, votes
	}
	return winner, votes
}

// voteFor builds the project context, calls Haiku, and parses the
// response into a Vote. Failures are returned in Vote.Err so the
// caller can log all per-project outcomes uniformly.
func voteFor(project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID}

	kb, err := readProjectKB(project.ID)
	if err != nil {
		// KB read failures are non-fatal — vote with empty KB. The
		// prompt already handles thin context via the calibration
		// language ("score LOW when uncertain").
		log.Printf("[classify] project %s: KB read failed (%v) — voting with empty KB", project.ID, err)
		kb = ""
	}

	desc := entity.Description
	if len(desc) > entityDescriptionMaxLen {
		desc = desc[:entityDescriptionMaxLen] + "\n…[truncated]"
	}

	prompt := fmt.Sprintf(
		classifyPrompt,
		project.Name,
		project.Description,
		kb,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		desc,
	)

	score, rationale, err := runHaiku(prompt)
	if err != nil {
		v.Err = err
		return v
	}
	v.Score = score
	v.Rationale = rationale
	return v
}

// pickWinner returns the highest-scoring above-threshold project_id, or
// nil if none qualify. Ties resolve to first-returned (slice order ==
// iteration order from db.ListProjects, which is alphabetical by name).
// Per the SKY-220 spec, a tie-breaking heuristic is out of scope for v1.
func pickWinner(votes []Vote) *string {
	bestScore := -1
	var winner string
	for _, v := range votes {
		if v.Err != nil {
			continue
		}
		if v.Score < ConfidenceThreshold {
			continue
		}
		if v.Score > bestScore {
			bestScore = v.Score
			winner = v.ProjectID
		}
	}
	if bestScore < 0 {
		return nil
	}
	return &winner
}

// readProjectKB returns the concatenated content of every .md file
// under the project's knowledge-base/ directory, capped at
// kbInlineMaxBytes total. Files are read in lexical order so the same
// inputs produce the same prompt across runs.
func readProjectKB(projectID string) (string, error) {
	root, err := curator.KnowledgeDir(projectID)
	if err != nil {
		return "", err
	}
	kbDir := filepath.Join(root, "knowledge-base")
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var buf bytes.Buffer
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(kbDir, name))
		if err != nil {
			log.Printf("[classify] project %s: skip KB file %s (%v)", projectID, name, err)
			continue
		}
		fmt.Fprintf(&buf, "## %s\n\n%s\n\n", name, data)
		if buf.Len() >= kbInlineMaxBytes {
			buf.WriteString("\n…[knowledge base truncated for prompt size]")
			break
		}
	}
	return buf.String(), nil
}

// runHaiku invokes the local `claude` CLI with the classify prompt
// and parses the response. Mirrors internal/ai/scorer.go:scoreBatch's
// subprocess shape — the CLI envelope wraps actual output in a
// {"result": "..."} object that we unwrap before parsing.
//
// Exposed as a var so tests can substitute a deterministic stub
// without spawning the local `claude` binary.
var runHaiku = realRunHaiku

func realRunHaiku(prompt string) (int, string, error) {
	cmd := exec.Command("claude",
		"-p", prompt,
		"--model", classifyModel,
		"--output-format", "json",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, "", fmt.Errorf("claude command failed: %w (stderr: %s)", err, stderr.String())
	}

	raw := stdout.Bytes()
	var envelope struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Result != "" {
		raw = []byte(envelope.Result)
	}
	raw = stripCodeFences(raw)

	var resp struct {
		Score     int    `json:"score"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, "", fmt.Errorf("parse classify response: %w (raw: %s)", err, string(raw))
	}
	if resp.Score < 0 {
		resp.Score = 0
	}
	if resp.Score > 100 {
		resp.Score = 100
	}
	return resp.Score, resp.Rationale, nil
}

func stripCodeFences(b []byte) []byte {
	s := bytes.TrimSpace(b)
	if bytes.HasPrefix(s, []byte("```")) {
		if idx := bytes.Index(s[3:], []byte("\n")); idx >= 0 {
			s = s[3+idx+1:]
		}
		if idx := bytes.LastIndex(s, []byte("```")); idx >= 0 {
			s = s[:idx]
		}
	}
	return bytes.TrimSpace(s)
}
