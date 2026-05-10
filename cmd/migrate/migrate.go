// Package migrate is the CLI entrypoint for the `triagefactory migrate`
// subcommand. It exposes the operator-facing slice of goose:
//
//	triagefactory migrate up      bring the schema to head
//	triagefactory migrate status  show applied / pending versions
//
// Down migrations are intentionally not exposed (see SKY-245's spec for
// the rationale — installed user-tools shouldn't ship a footgun for
// downgrade-induced data loss).
//
// The subcommand opens the same SQLite path the server does so an
// operator can run `triagefactory migrate status` against an existing
// install without spinning up the HTTP stack.
package migrate

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// Handle is the entrypoint dispatched from main.go on
// `triagefactory migrate ...`. The first argv after `migrate` is the
// sub-subcommand; anything else falls through to a usage print so
// operators get a quick reference rather than a silent no-op.
func Handle(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "up":
		runUp()
	case "status":
		runStatus()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown migrate subcommand %q\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

func runUp() {
	database, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.Migrate(database); err != nil {
		fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("migrations applied (schema at head)")
}

func runStatus() {
	database, err := db.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()
	if err := db.MigrationStatus(database, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "migrate status: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`triagefactory migrate — schema migration ops.

USAGE
  triagefactory migrate up        bring the schema to head
  triagefactory migrate status    list applied + pending migrations

NOTES
  Down migrations are intentionally not exposed; for installed
  user-tools, downgrade-induced data loss is a footgun without a
  matching upside. See SKY-245 for the design discussion.`)
}
