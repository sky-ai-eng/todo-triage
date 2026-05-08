package jira

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// ticketHelp maps each ticket subcommand to its usage line. Mirrored
// in jira.HelpText (the master overview); the per-action lookup here
// powers `jira ticket <action> --help`. Drift between the two is
// caught manually for now — they live in adjacent files and any
// rename has to touch both.
var ticketHelp = map[string]string{
	"view":             "jira ticket view <key>",
	"transition":       "jira ticket transition <key> --status <status>",
	"list-transitions": "jira ticket list-transitions <key>",
	"comment":          "jira ticket comment <key> --body <text>",
	"assign":           "jira ticket assign <key>",
	"unassign":         "jira ticket unassign <key>",
	"create":           "jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]",
	"edit":             "jira ticket edit <key> [--summary <text>] [--description <text>] [--priority <priority>] [--type <type>] [--add-label <label>] [--remove-label <label>]",
	"set-parent":       "jira ticket set-parent <key> --parent <parent_key>",
	"set-priority":     "jira ticket set-priority <key> --priority <priority>",
	"search":           "jira ticket search --jql <jql> [--fields <f1,f2,...>] [--max <N>]",
	"list-children":    "jira ticket list-children <key>",
	"list-types":       "jira ticket list-types <project>",
	"list-priorities":  "jira ticket list-priorities",
}

// hasHelpFlag returns true if --help or -h appears as a standalone
// flag (not as the value of a preceding --xxx flag). Per-action help
// dispatch trips on this BEFORE the action body runs so the agent
// can run e.g. `jira ticket view --help` without the leading arg
// being misread as the issue key.
//
// The "preceding flag" exclusion is what makes
// `jira ticket comment KEY --body "--help"` execute the comment
// instead of printing help — the literal "--help" is the body's
// value, not a help request. Same for a JQL search whose query
// happens to contain --help. Assumption: jira ticket has no boolean
// flags. If one is added, --xxxBool --help would be misread as the
// boolean's value; the assumption is enforced by ticketHelp listing
// only value-taking flags. Adding a boolean would require revisiting
// this helper.
func hasHelpFlag(args []string) bool {
	for i, a := range args {
		if a != "--help" && a != "-h" {
			continue
		}
		if i > 0 {
			prev := args[i-1]
			// A `--xxx` arg is treated as a value-taking flag and
			// the current --help is its value, not a help request.
			// `--help` itself doesn't claim a value, so an earlier
			// `--help` doesn't shadow this one.
			if strings.HasPrefix(prev, "--") && prev != "--help" {
				continue
			}
		}
		return true
	}
	return false
}

func handleTicket(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: triagefactory exec jira ticket <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	// Per-action --help: print just that action's usage and exit cleanly,
	// without invoking the action body (which would otherwise try to
	// interpret --help as an issue key / project / etc.).
	if hasHelpFlag(flags) {
		if h, ok := ticketHelp[action]; ok {
			fmt.Printf("usage: triagefactory exec %s\n", h)
			return
		}
		// Unknown action with --help — fall through to the unknown-action
		// error below so the user sees that the action itself is wrong.
	}

	switch action {
	case "view":
		ticketView(client, flags)
	case "transition":
		ticketTransition(client, flags)
	case "list-transitions":
		ticketListTransitions(client, flags)
	case "comment":
		ticketComment(client, flags)
	case "assign":
		ticketAssign(client, flags)
	case "unassign":
		ticketUnassign(client, flags)
	case "create":
		ticketCreate(client, flags)
	case "edit":
		ticketEdit(client, flags)
	case "set-parent":
		ticketSetParent(client, flags)
	case "list-types":
		ticketListTypes(client, flags)
	case "list-children":
		ticketListChildren(client, flags)
	case "search":
		ticketSearch(client, flags)
	case "list-priorities":
		ticketListPriorities(client)
	case "set-priority":
		ticketSetPriority(client, flags)
	default:
		exitErr(fmt.Sprintf("unknown ticket action: %s", action))
	}
}

func ticketView(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket view <key>")
	}
	issue, err := client.GetIssue(args[0])
	exitOnErr(err)
	printJSON(issue)
}

func ticketTransition(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket transition <key> --status <status>")
	}
	key := args[0]
	status := flagVal(args, "--status")
	if status == "" {
		exitErr("--status is required")
	}
	err := client.TransitionTo(key, status)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "status": status})
}

func ticketListTransitions(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-transitions <key>")
	}
	transitions, err := client.GetTransitions(args[0])
	exitOnErr(err)
	printJSON(transitions)
}

func ticketComment(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket comment <key> --body <text>")
	}
	key := args[0]
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	err := client.AddComment(key, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketAssign(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket assign <key>")
	}
	err := client.AssignToSelf(args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": "self"})
}

func ticketUnassign(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket unassign <key>")
	}
	err := client.Unassign(args[0])
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": args[0], "assigned": nil})
}

func ticketCreate(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket create <project> --type <type> --summary <text> [--description <text>] [--parent <key>] [--priority <priority>]")
	}
	project := args[0]
	issueType := flagVal(args, "--type")
	summary := flagVal(args, "--summary")
	description := flagVal(args, "--description")
	parentKey := flagVal(args, "--parent")
	priority := flagVal(args, "--priority")

	if issueType == "" {
		exitErr("--type is required")
	}
	if summary == "" {
		exitErr("--summary is required")
	}

	key, err := client.CreateIssue(project, issueType, summary, description, parentKey, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketEdit(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket edit <key> [--summary <text>] [--description <text>] [--priority <priority>] [--type <type>] [--add-label <label>] [--remove-label <label>]")
	}
	key := args[0]

	fields := jiraclient.UpdateIssueFields{}
	if flagPresent(args, "--summary") {
		v := flagVal(args, "--summary")
		fields.Summary = &v
	}
	if flagPresent(args, "--description") {
		v := flagVal(args, "--description")
		fields.Description = &v
	}
	if flagPresent(args, "--priority") {
		v := flagVal(args, "--priority")
		fields.Priority = &v
	}
	if flagPresent(args, "--type") {
		v := flagVal(args, "--type")
		fields.IssueType = &v
	}
	fields.AddLabels = flagVals(args, "--add-label")
	fields.RemoveLabels = flagVals(args, "--remove-label")

	if fields.IsEmpty() {
		exitErr("at least one of --summary, --description, --priority, --type, --add-label, --remove-label is required")
	}

	err := client.UpdateIssue(key, fields)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key})
}

func ticketSetParent(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-parent <key> --parent <parent_key>")
	}
	key := args[0]
	parentKey := flagVal(args, "--parent")
	if parentKey == "" {
		exitErr("--parent is required")
	}
	err := client.SetParent(key, parentKey)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "parent": parentKey})
}

func ticketListChildren(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-children <key>")
	}
	children, err := client.GetChildIssues(args[0])
	exitOnErr(err)
	if children == nil {
		children = []jiraclient.Issue{}
	}
	printJSON(children)
}

func ticketSearch(client *jiraclient.Client, args []string) {
	jql := flagVal(args, "--jql")
	if jql == "" {
		exitErr("--jql is required")
	}

	var fields []string
	if f := flagVal(args, "--fields"); f != "" {
		for _, field := range strings.Split(f, ",") {
			fields = append(fields, strings.TrimSpace(field))
		}
	}

	maxResults := 50
	if m := flagVal(args, "--max"); m != "" {
		v, err := strconv.Atoi(m)
		if err != nil {
			exitErr("--max must be a number")
		}
		maxResults = v
	}

	issues, err := client.SearchIssues(jql, fields, maxResults)
	exitOnErr(err)
	if issues == nil {
		issues = []jiraclient.Issue{}
	}
	printJSON(issues)
}

func ticketListPriorities(client *jiraclient.Client) {
	priorities, err := client.ListPriorities()
	exitOnErr(err)
	printJSON(priorities)
}

func ticketSetPriority(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket set-priority <key> --priority <priority>")
	}
	key := args[0]
	priority := flagVal(args, "--priority")
	if priority == "" {
		exitErr("--priority is required")
	}
	err := client.SetPriority(key, priority)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "key": key, "priority": priority})
}

func ticketListTypes(client *jiraclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: jira ticket list-types <project>")
	}
	types, err := client.ListIssueTypes(args[0])
	exitOnErr(err)
	printJSON(types)
}

// --- helpers ---

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// flagPresent reports whether `flag` appears anywhere in args. Used by
// `edit` to distinguish "flag omitted" (leave field untouched) from
// "flag passed with empty value" (set field to empty).
func flagPresent(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// flagVals collects every value of a repeatable flag (e.g. --add-label foo --add-label bar).
func flagVals(args []string, flag string) []string {
	var vals []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			vals = append(vals, args[i+1])
		}
	}
	return vals
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
