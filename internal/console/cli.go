package console

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// RunCLI implements the `lightstack console-tokens` subcommand (Article II).
//
// Protocol, matching the existing `cleanup` and `healthcheck` subcommands:
// text to stdout, --json for structured data, errors to stderr, and documented
// exit codes — 0 success, 1 operation failed, 2 usage error.
//
// This is the supported way to inspect and revoke console tokens. The projection
// files are readable by hand, but reading them tells you nothing about expiry or
// ownership, both of which live in the table.
func RunCLI(ctx context.Context, store *Store, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "list":
		return runList(ctx, store, args[1:], stdout, stderr)
	case "purge":
		return runPurge(ctx, store, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "console-tokens: unknown subcommand %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// NeedsStore reports whether args name a command that uses the store. RunCLI answers
// everything else — usage, help, an unknown command — with a nil store, so the caller
// can do so without loading a config or connecting. Keep it in step with RunCLI.
func NeedsStore(args []string) bool {
	return len(args) > 0 && (args[0] == "list" || args[0] == "purge")
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: lightstack console-tokens <command> [flags]

Inspect and revoke noVNC console tokens.

Commands:
  list    Show live console tokens (token values are redacted)
  purge   Remove expired tokens, or every token of one instance

Flags:
  list  --json                 emit JSON instead of a table
  purge --instance <uuid>      revoke every token of this instance
                               (default: remove expired tokens only)

Exit codes: 0 success, 1 operation failed, 2 usage error
`)
}

func runList(ctx context.Context, store *Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if store == nil {
		fmt.Fprintln(stderr, "console-tokens: no database connection")
		return 1
	}

	records, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "console-tokens: %v\n", err)
		return 1
	}

	if *asJSON {
		return outputListJSON(records, stdout, stderr)
	}
	return outputListTable(records, stdout, stderr)
}

// listRow is the JSON view of a console token.
//
// A separate type from Record on purpose: Token here is REDACTED. A JSON dump is
// exactly the sort of thing that ends up pasted into a bug report, and a full token
// is a live credential for someone's framebuffer until it expires.
type listRow struct {
	Token      string    `json:"token"`
	InstanceID string    `json:"instance_id"`
	ProjectID  string    `json:"project_id"`
	Socket     string    `json:"socket"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// outputListJSON emits the records as indented JSON with every token redacted.
func outputListJSON(records []Record, stdout, stderr io.Writer) int {
	out := make([]listRow, 0, len(records))
	for _, r := range records {
		out = append(out, listRow{
			Token:      redact(r.Token),
			InstanceID: r.InstanceID,
			ProjectID:  r.ProjectID,
			Socket:     r.Socket,
			CreatedAt:  r.CreatedAt,
			ExpiresAt:  r.ExpiresAt,
		})
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "console-tokens: %v\n", err)
		return 1
	}
	return 0
}

// outputListTable emits the human-readable table, tokens likewise redacted.
func outputListTable(records []Record, stdout, stderr io.Writer) int {
	if len(records) == 0 {
		fmt.Fprintln(stdout, "no live console tokens")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN\tINSTANCE\tTARGET\tEXPIRES IN")
	for _, r := range records {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			redact(r.Token), r.InstanceID, r.Socket,
			time.Until(r.ExpiresAt).Round(time.Second))
	}
	return flushOrFail(tw, stderr)
}

func runPurge(ctx context.Context, store *Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	instance := fs.String("instance", "", "revoke every token of this instance id")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if store == nil {
		fmt.Fprintln(stderr, "console-tokens: no database connection")
		return 1
	}

	if *instance != "" {
		if err := store.RevokeByInstance(ctx, *instance); err != nil {
			fmt.Fprintf(stderr, "console-tokens: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "revoked all console tokens for instance %s\n", *instance)
		return 0
	}

	n, err := store.RevokeExpired(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "console-tokens: %v\n", err)
		return 1
	}
	if err := store.SweepOrphans(ctx); err != nil {
		fmt.Fprintf(stderr, "console-tokens: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed %d expired console token(s)\n", n)
	return 0
}

func flushOrFail(tw *tabwriter.Writer, stderr io.Writer) int {
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "console-tokens: %v\n", err)
		return 1
	}
	return 0
}
