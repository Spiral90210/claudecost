# claudecost

Go CLI: ingests Claude Code transcripts (`~/.claude/projects/**/*.jsonl`) into SQLite, prices each API call, and reports cost via CLI, web dashboard (`serve`) and SwiftBar menu bar (`summary --format swiftbar`). User docs: README.md.

## Commands

- Build/check: `gofmt -l . ; go vet ./... && go build -o claudecost .`
- Dashboard: `go run . serve` (http://127.0.0.1:7777)
- Update the installed menu bar binary after changes: `go install .`
- No test suite. Check changes against a scratch database, never `~/.claude/cost.db`:
  - `./claudecost --db <scratch>/test.db ingest && ./claudecost --db <scratch>/test.db report --by model --days 0`
  - Dashboard: `serve --db <scratch>/test.db --addr 127.0.0.1:7788 --interval 0` (7777 may be the user's own instance), screenshot with headless Chrome (`--screenshot`, `--blink-settings=preferredColorScheme=0|1`; minimum viewport width is 500px)

## Files

- `main.go`: CLI dispatch, db and transcript path resolution
- `db.go`: schema (`files`, `usage`, `prices`, `settings`), connection setup
- `ingest.go`: incremental transcript reader
- `prices.go`: pricer, `prices` subcommands, `reprice`; `prices.json` is the embedded seed
- `report.go`, `summary.go`, `server.go` + `web/index.html` (embedded html/template)
- `swiftbar/claudecost.1m.sh`: plugin; install rewrites its `BIN=` line
- `price_update_skill.md`: Claude Code skill to update prices from the live pricing page

## Gotchas

- Prices: never from memory. Source is https://platform.claude.com/docs/en/about-claude/pricing.md. `prices.json` only seeds an empty `prices` table; existing databases change via `prices set/import`.
- Transcripts log each API response several times (one line per content block, output_tokens growing). Dedupe key is (message.id, requestId); upsert keeps MAX per token field and nulls cost, then `ingest` reprices `cost_usd IS NULL` rows. Unpriced models stay NULL, never $0.
- Ingest tracks a byte offset per file; a trailing line without `\n` is left for the next run; a file smaller than its offset is re-read from 0.
- `db.SetMaxOpenConns(1)`: never call `db.*` while a `tx` is open (deadlock). Load anything needed before `db.Begin()`.
- Transactions are `_txlock=immediate` so concurrent ingests (serve + menu bar + CLI) queue; offsets are read inside the tx. Setup retries SQLITE_BUSY because a brand-new file isn't in WAL mode yet.
- `ts` is stored as UTC RFC3339; days and hours use `date(ts, 'localtime')`. `effective_from` (YYYY-MM-DD) compares to `ts` as a string.
- Effort uses `per_turn_effort`, falling back to `effort` (the difference between the two transcript fields is unconfirmed).
- Dashboard: colors via CSS classes (`s-opus`, `h3`), not template values in `style` (html/template CSS escaping yields `ZgotmplZ`). No external requests; system fonts only. Chart colors follow model family, not rank.
- Projects group by repo: `<repo>/.claude/worktrees/...` maps to `<repo>`, subdirectories to the nearest ancestor with `.git`.
- SwiftBar runs with a bare PATH and no shell profile: absolute binary path, and `--db` in the script if not using the default.
