# claudecost

Track Claude Code cost, model and effort usage from your local transcripts, with a web dashboard and a menu bar item.

## Run the dashboard

```sh
go run . serve
```

Open http://127.0.0.1:7777. It reads new transcript lines on start and every minute after.

## Install the menu bar plugin

Requires [SwiftBar](https://github.com/swiftbar/SwiftBar) (`brew install --cask swiftbar`, then choose a plugin folder). From the repo root:

```sh
go install .
BIN="$(go env GOBIN)"; [ -n "$BIN" ] || BIN="$(go env GOPATH | cut -d: -f1)/bin"
PLUGINS="$(defaults read com.ameba.SwiftBar PluginDirectory)"
sed "s|^BIN=.*|BIN=\"$BIN/claudecost\"|" swiftbar/claudecost.1m.sh > "$PLUGINS/claudecost.1m.sh"
chmod +x "$PLUGINS/claudecost.1m.sh"
```

After code changes, run `go install .` again. The plugin copy only needs reinstalling if `swiftbar/claudecost.1m.sh` changes.

## Install the price update skill

```sh
mkdir -p ~/.claude/skills/update-prices
cp price_update_skill.md ~/.claude/skills/update-prices/SKILL.md
```

For this project only, copy it to `.claude/skills/update-prices/SKILL.md` in the repo instead. Then run `/update-prices` in Claude Code, or ask it to "check claudecost prices".

## How the skill sources prices

The skill never uses prices from memory. Each run:

1. **Snapshots the current state:** `claudecost prices export` and `claudecost report --by model --days 0`, which also lists any unpriced models.
2. **Fetches the live pricing page:** `https://platform.claude.com/docs/en/about-claude/pricing.md`. From it, it extracts per model, in USD per million tokens:
   - base input, 5m cache write, 1h cache write, cache read, output
   - fast mode rates. The page gives only fast input and output, so fast cache rates are derived as fast input x that model's cache multipliers from the same page.
   - prompt-size tiers, e.g. Haiku 5.5 above 100,000 tokens, stored as a row with `min_prompt` = threshold + 1
   - settings: the US-only inference multiplier (`geo_us_multiplier`) and the web search fee (`web_search_per_1k`)
   - any effective date the page states for a change
3. **Classifies each difference:**
   - *New model:* added with `effective_from` 2000-01-01, so all history is priced.
   - *Correction:* the existing row is updated in place.
   - *Dated price change:* a new row from that date. Older calls keep the old price.
   - If the page doesn't say which, the skill asks you.
4. **Shows you a table of proposed changes** and waits for confirmation.
5. **Imports only the changed rows** with `claudecost prices import`. Each row's `source` records the page and the date it was read. The import reprices affected calls.
6. **Reports** rows changed, calls repriced, total cost before and after, and any models still unpriced.

A model the page doesn't list is reported as unpriced, never guessed. The default `prices.json` shipped in the repo was built the same way, from the pricing page on 2026-10-31.

## Dashboard

- today, last 7 days, month to date, and average per active day, each with token totals
- daily cost stacked by model family with a 7-day average line; hover a day for its model and token breakdown
- cost by model, effort and project (worktrees and subdirectories are grouped under their git repo)
- cost by token type, cache hit rate, net savings from caching, subagent share
- an hour x weekday heatmap and the most expensive sessions

Ranges: 7, 30 or 90 days (`/?range=7`). The page reloads itself when new usage arrives and follows the system light/dark setting. It loads nothing external: fonts are system fonts (SF Mono in Safari, Menlo in Chrome, system sans for prose).

Flags: `--addr` (default `127.0.0.1:7777`), `--interval` (default `1m`, `0` disables periodic ingest), `--projects`.

## Menu bar

Every minute the plugin reads new transcript lines and shows today's cost in the menu bar. The dropdown has:
- today, last 7 days and month to date
- today's cost by model and by effort
- today's tokens
- a link to the dashboard when `serve` is running
- an "Ingest now" action

The icon becomes a warning triangle when calls are unpriced or the ingest failed.

To change the refresh interval, rename the installed file (e.g. `claudecost.5m.sh`). SwiftBar does not load your shell profile, so if your database is not at `~/.claude/cost.db`, add `--db PATH` to the last line of the installed script.

The menu bar, `serve` and manual `ingest` can run at once. Each ingest waits for the database write lock and continues from where the previous one stopped.

## CLI

```sh
claudecost ingest                          # read new transcript lines
claudecost summary                         # today, 7 days, month to date (what the menu bar shows)
claudecost report --by day --days 30       # also: model, effort, project, session; --days 0 = all
claudecost prices list                     # current price table
claudecost reprice                         # recompute every call's cost
claudecost serve                           # dashboard
```

| What | Resolution order |
|---|---|
| Database | `--db PATH`, `$CLAUDECOST_DB`, `~/.claude/cost.db` |
| Transcripts | `--projects DIR`, `$CLAUDE_CONFIG_DIR/projects`, `~/.claude/projects` |

Requires Go 1.27+. Pure Go (no cgo).

## How cost is calculated

- Raw token counts are stored per API call: input, output (including thinking), cache writes (5m and 1h), cache reads, web searches.
- Prices live in the `prices` table (USD per million tokens). There is one row per model, speed (`standard` / `fast`), prompt-size tier and `effective_from` date. Defaults come from `prices.json` and are loaded on first run.
- Each call uses the newest row in effect at its timestamp, so calls stay priced at the rate that applied when they were made.
- `inference_geo: "us"` applies `geo_us_multiplier`. Web searches add `web_search_per_1k` per 1,000.
- A model with no price row is left unpriced, not $0. `report`, the dashboard and the menu bar warn about it, and the next ingest prices it once a row exists.
- Transcripts log each API response several times. Each response is stored once, with the highest token counts seen.

## Changing prices by hand

```sh
# a price change from a date: add a row (older calls keep the old price)
claudecost prices set --model claude-opus-5-5 --from 2026-12-01 \
  --input 4 --output 20 --cw5m 5 --cw1h 8 --cache-read 0.2 --source "pricing page 2026-12-01"

# bulk: export, edit, import
claudecost prices export > my-prices.json
claudecost prices import my-prices.json
```

Both reprice the affected calls immediately.
