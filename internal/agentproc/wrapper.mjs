// Triage Factory Agent SDK shim. Spawned by internal/agentproc.Run as
// `node wrapper.mjs <flags>` in place of `claude -p ...`. Translates the
// flag-based argv that BuildArgs (argv.go) emits into Agent SDK Options
// and pipes the query() event stream to stdout in NDJSON, matching the
// `--output-format stream-json --verbose` shape the Go-side parser
// (stream.go) already consumes.
//
// Auth resolves through the SDK's own env-var/keychain logic — we
// deliberately don't touch ANTHROPIC_API_KEY, CLAUDE_CODE_USE_BEDROCK,
// AWS creds, or the macOS keychain entry here. Whatever the parent
// process (Triage Factory binary) put in env wins, and the SDK
// transparently falls back to the user's local Claude Code OAuth login
// when nothing is set, billing against their Pro/Max subscription.
import { query } from "@anthropic-ai/claude-agent-sdk"

function parseArgs(argv) {
  let prompt = ""
  const opts = {}
  const addDirs = []

  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i]
    const next = () => argv[++i]
    switch (flag) {
      case "-p":
        prompt = next()
        break
      case "--resume":
        opts.resume = next()
        break
      case "--model":
        opts.model = next()
        break
      case "--allowedTools":
        opts.allowedTools = next().split(",").filter(Boolean)
        break
      case "--add-dir":
        addDirs.push(next())
        break
      case "--append-system-prompt":
        // Preserve Claude Code's default system prompt and append the
        // runtime-specific text. Mirrors the CLI's --append-system-prompt
        // behavior; replacing the prompt entirely would clobber CC's
        // tool-use defaults.
        opts.systemPrompt = {
          type: "preset",
          preset: "claude_code",
          append: next(),
        }
        break
      case "--max-turns":
        opts.maxTurns = parseInt(next(), 10)
        break
      case "--output-format":
        // CLI flag — the SDK iterator already emits the same shape.
        next()
        break
      case "--verbose":
        // CLI flag — SDK is always verbose at this level.
        break
      default:
        process.stderr.write(`wrapper: ignoring unknown arg ${flag}\n`)
    }
  }

  if (addDirs.length > 0) opts.additionalDirectories = addDirs
  return { prompt, options: opts }
}

const { prompt, options } = parseArgs(process.argv.slice(2))

if (!prompt) {
  process.stderr.write("wrapper: missing -p <message>\n")
  process.exit(2)
}

try {
  for await (const msg of query({ prompt, options })) {
    process.stdout.write(JSON.stringify(msg) + "\n")
  }
} catch (err) {
  process.stderr.write(`wrapper error: ${err?.stack ?? err}\n`)
  process.exit(1)
}
