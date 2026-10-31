package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

const usageText = `claudecost - track Claude Code cost from local transcripts

usage:
  claudecost [--db PATH] <command> [flags]

commands:
  ingest            read new transcript lines into the database
  report            print cost breakdowns (--by day|model|effort|project, --days N)
  prices list       show the price table (--json for machine output)
  prices set        add or update a price row, then reprice that model
  prices import F   upsert prices from a JSON file, then reprice
  prices export     print the price table as JSON (same format as import)
  reprice           recompute cost for every row (--model to limit)
  serve             web dashboard (--addr 127.0.0.1:7777, --interval 1m)
  summary           ingest, then print today/7d/month (--format text|swiftbar)

db path: --db, else $CLAUDECOST_DB, else ~/.claude/cost.db
transcripts: --projects, else $CLAUDE_CONFIG_DIR/projects, else ~/.claude/projects
`

func main() {
	global := flag.NewFlagSet("claudecost", flag.ExitOnError)
	dbPath := global.String("db", "", "database path")
	global.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	global.Parse(os.Args[1:])
	args := global.Args()
	if len(args) == 0 {
		global.Usage()
		os.Exit(2)
	}

	path := resolveDB(*dbPath)
	db, err := openDB(path)
	if err != nil {
		fatal(err)
	}
	defer db.Close()

	switch args[0] {
	case "ingest":
		err = cmdIngest(db, args[1:])
	case "report":
		err = cmdReport(db, args[1:])
	case "prices":
		err = cmdPrices(db, args[1:])
	case "reprice":
		err = cmdReprice(db, args[1:])
	case "serve":
		err = cmdServe(db, path, args[1:])
	case "summary":
		err = cmdSummary(db, args[1:])
	default:
		global.Usage()
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func resolveDB(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("CLAUDECOST_DB"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "cost.db")
}

func resolveProjects(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
