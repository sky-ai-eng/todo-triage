// Package chain implements the `triagefactory exec chain` CLI surface —
// agent-callable commands that the chain orchestrator reads to decide
// whether a chain proceeds to the next step or aborts.
//
// The flow: a chain step's wrapper user prompt instructs the agent to
// call `chain verdict --proceed|--abort --reason ...` before emitting
// its completion envelope. The verdict lands in run_artifacts with
// kind='chain:verdict' and a JSON metadata blob. The orchestrator
// reads the latest verdict for the step's run after Claude terminates;
// no verdict means abort with reason "no-verdict".
package chain

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// HelpText is the help block for `chain` commands.
const HelpText = `Chain Commands:
  chain verdict --proceed --reason <text> [--notes <text>]
  chain verdict --abort   --reason <text> [--notes <text>]
  chain verdict --final   --reason <text> [--notes <text>]

Records the chain step's verdict. Read by the orchestrator after the
step's completion envelope. Exactly one of --proceed / --abort / --final
must be set. The verdict is persisted to run_artifacts(kind='chain:verdict');
the orchestrator picks the most recent verdict written by the step.

  --proceed  advance to the next step
  --abort    stop the chain; leave the task open for human inspection
  --final    end the chain successfully at this step; close the task.
             The step may take one terminal external action (post a
             review, comment, or PR) before recording --final; that
             action still passes through the standard human-approval
             gate. Use this when the step decides the chain's intended
             outcome can be achieved here without further steps (e.g.,
             a preflight that posts a SKIP review and exits).

Idempotency: re-running this command in the same step appends a new
verdict artifact; the orchestrator reads the most recent. Use this if
you want to revise a verdict before emitting the completion envelope.

Run id is read from $TRIAGE_FACTORY_RUN_ID (set by the delegation
spawner). The command refuses to run when invoked outside a chain
step (the run has no chain_run_id).`

// Handle dispatches chain subcommands.
func Handle(chains db.ChainStore, args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return
	}
	switch args[0] {
	case "verdict":
		runVerdict(chains, args[1:])
	default:
		exitErr("unknown chain command: " + args[0])
	}
}

func printHelp() {
	fmt.Printf("Usage: triagefactory exec chain <command> [args]\n\n%s\n", HelpText)
}

func runVerdict(chains db.ChainStore, args []string) {
	fs := flag.NewFlagSet("chain verdict", flag.ContinueOnError)
	var (
		proceed bool
		abort   bool
		final   bool
		reason  string
		notes   string
	)
	fs.BoolVar(&proceed, "proceed", false, "advance the chain to the next step")
	fs.BoolVar(&abort, "abort", false, "stop the chain (leave task open)")
	fs.BoolVar(&final, "final", false, "end the chain successfully at this step (close task)")
	fs.StringVar(&reason, "reason", "", "one-line reason (required)")
	fs.StringVar(&notes, "notes", "", "optional longer notes")
	if err := fs.Parse(args); err != nil {
		exitErr("parse flags: " + err.Error())
	}
	set := 0
	if proceed {
		set++
	}
	if abort {
		set++
	}
	if final {
		set++
	}
	if set != 1 {
		exitErr("exactly one of --proceed / --abort / --final is required")
	}
	if reason == "" {
		exitErr("--reason is required")
	}

	runID := os.Getenv("TRIAGE_FACTORY_RUN_ID")
	if runID == "" {
		exitErr("TRIAGE_FACTORY_RUN_ID not set; chain verdict can only be recorded inside a delegation run")
	}

	ctx := context.Background()
	chainRun, stepIdx, err := chains.GetRunForRun(ctx, runmode.LocalDefaultOrg, runID)
	if err != nil {
		exitErr("lookup chain run: " + err.Error())
	}
	if chainRun == nil {
		exitErr("this run is not part of a chain (no chain_run_id on the run row)")
	}
	if chainRun.Status != domain.ChainRunStatusRunning {
		exitErr(fmt.Sprintf("chain run %s is %s; cannot record a verdict", chainRun.ID, chainRun.Status))
	}

	var outcome domain.ChainVerdictOutcome
	switch {
	case proceed:
		outcome = domain.ChainVerdictAdvance
	case abort:
		outcome = domain.ChainVerdictAbort
	case final:
		outcome = domain.ChainVerdictFinal
	}
	verdict := domain.ChainVerdict{
		Outcome: outcome,
		Reason:  reason,
		Notes:   notes,
	}
	payload, err := json.Marshal(verdict)
	if err != nil {
		exitErr("encode verdict: " + err.Error())
	}
	if err := chains.InsertVerdict(ctx, runmode.LocalDefaultOrg, runID, string(payload)); err != nil {
		exitErr("record verdict: " + err.Error())
	}

	out := map[string]interface{}{
		"recorded": true,
		"step":     stepIdx,
		"outcome":  outcome,
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(out); err != nil {
		exitErr("encode response: " + err.Error())
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
