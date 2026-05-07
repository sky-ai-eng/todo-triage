// Package projectclassify decides which project (if any) a newly-
// discovered entity belongs to. SKY-220.
//
// Two-stage design:
//
//	Stage 1 — broad pass, fast.
//	  Per project, parallel, single-shot Haiku. Prompt inlines name +
//	  description + KB content truncated at kbInlineMaxBytes. Returns
//	  {score, rationale, kb_truncated}. ~1 model turn per project.
//
//	Stage 2 — selective deepening, only when warranted.
//	  Trigger: no Stage 1 winner above ConfidenceThreshold AND ≥1
//	  project scored borderlineMin..ConfidenceThreshold with a
//	  truncated KB. Re-classify all such projects in agent-mode:
//	  cmd.Dir = curator.KnowledgeDir(project.ID), --max-turns 6,
//	  prompt directs at ./knowledge-base/ via Read/Glob/Grep. The
//	  agent may take 3-8 model turns to selectively read what's
//	  relevant.
//
// Single-entity-per-call rather than batched. Discoveries are rare
// (a few per poll cycle at most) and each call already inlines the
// per-project context, so batching across entities would just
// duplicate context.
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

//go:embed prompts/classify_stage1.txt
var stage1Prompt string

//go:embed prompts/classify_stage2.txt
var stage2Prompt string

// ConfidenceThreshold is the minimum Haiku score (0-100) required to
// auto-assign an entity to a project. Below this, project_id stays
// NULL and the entity resurfaces in future project-creation backfill
// popups. 60 is a launch default; tune from real votes.
const ConfidenceThreshold = 60

// borderlineMin is the lower bound on Stage 1 scores that qualify for
// Stage 2 escalation. A project that scored 40-59 with a truncated KB
// might have crossed threshold if Haiku had been able to see the
// whole KB; below 40, escalation is unlikely to flip the outcome and
// the agent loop isn't worth the cost.
const borderlineMin = 40

// classifyModel mirrors the scorer's model choice — fast and cheap, and
// we want consistent Haiku-version drift across the two AI surfaces.
const classifyModel = "haiku"

// maxConcurrentVotes caps the number of in-flight Haiku calls per
// stage. Most installs have <10 projects so the cap rarely fires;
// the bound exists to avoid swamping the local `claude` CLI on
// pathological setups.
const maxConcurrentVotes = 8

// stage2MaxTurns caps how many tool-using turns the Stage 2 agent
// gets. 6 is enough for "Glob to see the layout, Read 2-3 files,
// answer" — anything more is the model wandering, which we'd rather
// abort than pay for.
const stage2MaxTurns = 6

// entityDescriptionMaxLen mirrors the scorer's truncation policy
// (internal/ai/scorer.go:descriptionMaxLen). The classifier prompt
// already includes title; the description is supplementary context
// that doesn't need to be unbounded.
const entityDescriptionMaxLen = 1500

// kbInlineMaxBytes caps the per-project knowledge-base content sent
// inline to Stage 1. Curator-written KBs typically fit easily; the
// cap exists for the pathological case where a user dumps a large
// reference document. Above this we truncate with a sentinel and let
// Stage 2 escalate if the score lands borderline.
const kbInlineMaxBytes = 30 * 1024

// Vote is the per-project result of one classification call.
type Vote struct {
	ProjectID   string
	Score       int
	Rationale   string
	KBTruncated bool // Stage 1 only: signals Stage 2 escalation candidacy.
	Stage       int  // 1 or 2 — useful for logging which path produced a Vote.
	Err         error
}

// Classify runs the per-project quorum vote for one entity. Returns
// the winning project_id (or nil if all votes are below threshold)
// plus the merged per-project vote slice (Stage 2 vote replaces
// Stage 1 vote for any project that re-classified).
//
// Stage 2 fires only when Stage 1 has no winner AND ≥1 borderline-
// truncated vote exists. In the common case (clear winner OR clearly
// nobody fits), Stage 2 is skipped and the cost stays at
// 1 turn × N projects.
func Classify(projects []domain.Project, entity domain.Entity) (*string, []Vote) {
	if len(projects) == 0 {
		return nil, nil
	}

	stage1 := runVotes(projects, entity, voteStage1)

	if winner := pickWinner(stage1); winner != nil {
		return winner, stage1
	}

	// Find projects that are borderline AND ran into the inline KB cap.
	// Anything not truncated already had its full KB at Stage 1, so re-
	// running with filesystem access wouldn't change the answer.
	var escalated []domain.Project
	for i, v := range stage1 {
		if v.Err != nil {
			continue
		}
		if v.Score < borderlineMin || v.Score >= ConfidenceThreshold {
			continue
		}
		if !v.KBTruncated {
			continue
		}
		escalated = append(escalated, projects[i])
	}
	if len(escalated) == 0 {
		return nil, stage1
	}

	log.Printf("[classify] entity %s: escalating %d borderline+truncated project(s) to Stage 2", entity.ID, len(escalated))
	stage2 := runVotes(escalated, entity, voteStage2)

	merged := mergeStages(stage1, stage2)
	return pickWinner(merged), merged
}

// runVotes fans out one Haiku call per project using the provided
// vote function (Stage 1 or Stage 2). Concurrency is capped at
// maxConcurrentVotes.
func runVotes(projects []domain.Project, entity domain.Entity, vote func(domain.Project, domain.Entity) Vote) []Vote {
	sem := make(chan struct{}, maxConcurrentVotes)
	votes := make([]Vote, len(projects))
	var wg sync.WaitGroup
	for i, p := range projects {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, project domain.Project) {
			defer wg.Done()
			defer func() { <-sem }()
			votes[idx] = vote(project, entity)
		}(i, p)
	}
	wg.Wait()
	return votes
}

// mergeStages produces the final per-project vote slice: for any
// project that re-classified in Stage 2, that result wins; for the
// rest, the Stage 1 result stands. The returned slice has one entry
// per project from the Stage 1 input, in original order.
func mergeStages(stage1, stage2 []Vote) []Vote {
	stage2ByID := make(map[string]Vote, len(stage2))
	for _, v := range stage2 {
		stage2ByID[v.ProjectID] = v
	}
	merged := make([]Vote, len(stage1))
	for i, v := range stage1 {
		if s2, ok := stage2ByID[v.ProjectID]; ok {
			merged[i] = s2
		} else {
			merged[i] = v
		}
	}
	return merged
}

// pickWinner returns the highest-scoring above-threshold project_id,
// or nil if none qualify. Ties resolve to first-returned (slice order
// == iteration order from db.ListProjects, which is alphabetical by
// name). Per the SKY-220 spec, a tie-breaking heuristic is out of
// scope for v1.
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

// voteStage1 is the broad-pass single-shot Haiku call. KB inlined
// up to kbInlineMaxBytes; flag KBTruncated when the cap was hit so
// the orchestrator knows whether Stage 2 might help.
func voteStage1(project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID, Stage: 1}

	kb, truncated, err := readProjectKB(project.ID)
	if err != nil {
		log.Printf("[classify] project %s stage 1: KB read failed (%v) — voting with empty KB", project.ID, err)
		kb = ""
	}
	v.KBTruncated = truncated

	prompt := fmt.Sprintf(
		stage1Prompt,
		project.Name,
		project.Description,
		kb,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		truncateDescription(entity.Description),
	)

	score, rationale, err := runStage1Haiku(prompt)
	if err != nil {
		v.Err = err
		return v
	}
	v.Score = score
	v.Rationale = rationale
	return v
}

// voteStage2 is the agent-mode call: Haiku in the project's working
// directory, free to Read/Glob/Grep `./knowledge-base/` selectively.
// Used only for borderline+truncated projects from Stage 1.
func voteStage2(project domain.Project, entity domain.Entity) Vote {
	v := Vote{ProjectID: project.ID, Stage: 2}

	kbRoot, err := curator.KnowledgeDir(project.ID)
	if err != nil {
		v.Err = fmt.Errorf("resolve project dir: %w", err)
		return v
	}
	if err := os.MkdirAll(kbRoot, 0o755); err != nil {
		v.Err = fmt.Errorf("ensure project dir: %w", err)
		return v
	}

	prompt := fmt.Sprintf(
		stage2Prompt,
		project.Name,
		project.Description,
		entity.Source,
		entity.SourceID,
		entity.Kind,
		entity.Title,
		truncateDescription(entity.Description),
	)

	score, rationale, err := runStage2Haiku(prompt, kbRoot)
	if err != nil {
		v.Err = err
		return v
	}
	v.Score = score
	v.Rationale = rationale
	return v
}

func truncateDescription(desc string) string {
	if len(desc) <= entityDescriptionMaxLen {
		return desc
	}
	return desc[:entityDescriptionMaxLen] + "\n…[truncated]"
}

// readProjectKB returns the concatenated content of every .md file
// under the project's knowledge-base/ directory, capped at
// kbInlineMaxBytes total. Files are read in lexical order so the same
// inputs produce the same prompt across runs. Returns (content,
// truncated, err) — truncated is true when the cap was hit, signaling
// the orchestrator that Stage 2 might help.
func readProjectKB(projectID string) (string, bool, error) {
	root, err := curator.KnowledgeDir(projectID)
	if err != nil {
		return "", false, err
	}
	kbDir := filepath.Join(root, "knowledge-base")
	entries, err := os.ReadDir(kbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
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
	truncated := false
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(kbDir, name))
		if err != nil {
			log.Printf("[classify] project %s: skip KB file %s (%v)", projectID, name, err)
			continue
		}
		fmt.Fprintf(&buf, "## %s\n\n%s\n\n", name, data)
		if buf.Len() >= kbInlineMaxBytes {
			truncated = true
			buf.WriteString("\n…[knowledge base truncated for prompt size — Stage 2 will read selectively]")
			break
		}
	}
	return buf.String(), truncated, nil
}

// runStage1Haiku invokes the local `claude` CLI for a single-shot
// classification. Mirrors internal/ai/scorer.go:scoreBatch's
// subprocess shape. Exposed as a var so tests can stub.
var runStage1Haiku = realRunStage1Haiku

func realRunStage1Haiku(prompt string) (int, string, error) {
	cmd := exec.Command("claude",
		"-p", prompt,
		"--model", classifyModel,
		"--output-format", "json",
	)
	return runHaiku(cmd)
}

// runStage2Haiku invokes the local `claude` CLI in the project's
// working directory so the model can use Read/Glob/Grep to inspect
// the KB selectively. --max-turns is a hard cap on the tool loop.
// Exposed as a var so tests can stub.
var runStage2Haiku = realRunStage2Haiku

func realRunStage2Haiku(prompt, cwd string) (int, string, error) {
	cmd := exec.Command("claude",
		"-p", prompt,
		"--model", classifyModel,
		"--output-format", "json",
		"--max-turns", fmt.Sprintf("%d", stage2MaxTurns),
	)
	cmd.Dir = cwd
	return runHaiku(cmd)
}

// runHaiku is the shared subprocess + JSON-parsing path for both
// stages. Stage 1 and Stage 2 differ only in their command setup
// (cwd, max-turns); the response shape is identical.
func runHaiku(cmd *exec.Cmd) (int, string, error) {
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
