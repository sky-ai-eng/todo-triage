package agentproc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunOptions configures one `claude -p` invocation. Callers populate
// every field they care about; zero-values fall back to claude's
// defaults (model unset, no resume, default max turns).
type RunOptions struct {
	// Cwd is the working directory the subprocess runs in.
	Cwd string

	// Model is passed via --model. Empty omits the flag.
	Model string

	// SessionID, when non-empty, switches the invocation to
	// `--resume <id>`. Used for the memory-gate retry loop, the
	// SKY-139 yield-resume flow, and the curator's per-message
	// resumption against a long-lived project session.
	SessionID string

	// Message is the value passed to `-p`. For an initial invocation
	// this is the full prompt (mission + envelope); for a resume it's
	// just the new user turn.
	Message string

	// AllowedTools is the comma-joined --allowedTools value. Callers
	// build this themselves (see internal/delegate.BuildAllowedTools)
	// — different runtimes have different threat models.
	AllowedTools string

	// MaxTurns sets --max-turns. Zero omits the flag.
	MaxTurns int

	// ExtraEnv is appended to os.Environ() for the subprocess. Use
	// this for run-scoped variables like TRIAGE_FACTORY_RUN_ID and
	// TRIAGE_FACTORY_REPO that the delegated CLI subcommands read.
	ExtraEnv []string

	// TraceID is stamped onto every emitted message's RunID field.
	// Storage-neutral: delegate uses the agent run UUID, the curator
	// uses its own message-group id.
	TraceID string
}

// Sink is the storage-side adapter that turns parsed stream events
// into rows + websocket pushes. Implementations are constructed per
// invocation (they typically close over a runID or projectID) and are
// not concurrency-safe — Run drives the sink from a single goroutine.
type Sink interface {
	// OnSession fires once, the first time the stream emits a
	// system/init event with a session_id. Implementations persist
	// the id to whatever table owns "this conversation's resume key"
	// (runs.session_id for delegate; projects.curator_session_id
	// for the curator). Returning an error is logged but does not
	// abort the run — the stream continues and the result still
	// lands; callers can re-attempt session capture on resume.
	OnSession(sessionID string) error

	// OnMessage fires per fully-accumulated assistant or tool message.
	// Implementations insert + broadcast. Returning an error is
	// logged and skipped; the run does not abort because a single
	// row failed to insert.
	OnMessage(msg *domain.AgentMessage) error
}

// Outcome bundles what Run observed: the terminal Result (nil if no
// `result` event was seen), the captured session id (empty if the
// stream never emitted system/init), and the captured stderr buffer
// (full — callers truncate for display).
//
// SessionID is exposed here in addition to flowing through Sink.OnSession
// so callers that need it post-run (memory-gate retry, takeover
// validation) don't have to plumb their own capture into the sink.
type Outcome struct {
	Result    *Result
	SessionID string
	Stderr    string
}

// Run spawns `claude` with the given options, pumps the stream-json
// output through Sink, and waits for the subprocess to exit.
//
// Cancellation: when ctx is cancelled mid-run, the goroutine sends
// SIGKILL to the entire process group (Setpgid is used so child
// processes the agent spawned go down with it), then waits for the
// subprocess to exit and returns ctx.Err().
//
// Error semantics:
//   - nil error + non-nil Outcome.Result: normal termination; caller
//     processes the Result (memory gate, completion JSON, etc.).
//   - nil error + nil Outcome.Result: subprocess exited cleanly
//     without emitting a `result` event. Treat as involuntary failure.
//   - non-nil error: argv-build / Start failure, stream malformed
//     mid-stream, subprocess crashed, or ctx cancelled. Outcome.Stderr
//     is populated when the subprocess produced any.
func Run(ctx context.Context, opts RunOptions, sink Sink) (*Outcome, error) {
	cmd := exec.Command("claude", BuildArgs(opts)...)
	cmd.Dir = opts.Cwd
	cmd.Env = append(os.Environ(), opts.ExtraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return &Outcome{Stderr: stderrBuf.String()}, fmt.Errorf("start claude: %w", err)
	}

	pgid := cmd.Process.Pid
	go func() {
		<-ctx.Done()
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			// Best-effort; subprocess may have already exited.
			_ = err
		}
	}()

	stream := NewStreamState()
	result, streamErr := consumeStream(stdout, sink, stream, opts.TraceID)

	waitErr := cmd.Wait()

	outcome := &Outcome{
		Result:    result,
		SessionID: stream.SessionID(),
		Stderr:    stderrBuf.String(),
	}

	// Stream-level malformation with no terminal result is the
	// stronger signal — surface it before any wait error.
	if streamErr != nil && result == nil {
		return outcome, fmt.Errorf("stream: %w", streamErr)
	}

	// Wait without a captured result is involuntary failure; let the
	// caller distinguish cancel from crash via ctx.Err().
	if waitErr != nil && result == nil {
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}
		return outcome, fmt.Errorf("claude exited with error: %w", waitErr)
	}

	return outcome, nil
}

// consumeStream scans NDJSON output, drives the Sink, and returns the
// first `result` event it sees. Sink errors are logged and skipped so
// a transient DB hiccup on one row doesn't abandon the whole run.
//
// Session id is delivered to the sink the first time it appears, not
// at stream close — any mid-run consumer (the future curator UI, a
// memory-gate retry, a takeover) can read it without waiting for the
// stream to complete.
func consumeStream(stdout io.Reader, sink Sink, stream *StreamState, traceID string) (*Result, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	sessionDelivered := false

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		messages, result := stream.ParseLine(line, traceID)

		if !sessionDelivered {
			if sid := stream.SessionID(); sid != "" {
				if err := sink.OnSession(sid); err != nil {
					log.Printf("[agentproc] sink.OnSession failed: %v", err)
				}
				sessionDelivered = true
			}
		}

		for _, msg := range messages {
			if err := sink.OnMessage(msg); err != nil {
				log.Printf("[agentproc] sink.OnMessage failed: %v", err)
				continue
			}
		}

		if result != nil {
			return result, nil
		}
	}
	return nil, scanner.Err()
}
