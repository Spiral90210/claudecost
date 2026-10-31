# claudecost

Track Claude Code cost from the local transcripts in `~/.claude/projects`, with breakdowns by day, model, effort, project and session. No API or console access needed.

Transcripts are read incrementally into a SQLite database: each run only reads lines added since the last run.

## Build

```sh
go build -o claudecost .
```

Requires Go 1.27+. Pure Go (no cgo), so the binary copies anywhere.

## Usage

```sh
claudecost ingest                          # read new transcript lines
claudecost report --by day --days 30       # also: model, effort, project, session; --days 0 = all
claudecost prices list                     # current price table
claudecost reprice                         # recompute every call's cost
```

Paths:

| What | Resolution order |
|---|---|
| Database | `--db PATH`, `$CLAUDECOST_DB`, `~/.claude/cost.db` |
| Transcripts | `ingest --projects DIR`, `$CLAUDE_CONFIG_DIR/projects`, `~/.claude/projects` |

## How cost is calculated

- Raw token counts are stored per API call: input, output (including thinking), cache writes (5m and 1h), cache reads, web searches.
- Prices live in the `prices` table (USD per million tokens), one row per model, speed (`standard` / `fast`), prompt-size tier and `effective_from` date. Defaults come from `prices.json` and are loaded on first run.
- Each call uses the newest row in effect at its timestamp. Calls stay priced at the rate that applied when they were made.
- `inference_geo: "us"` applies `geo_us_multiplier`; web searches add `web_search_per_1k` per 1,000.
- A model with no price row is left unpriced (not $0). `report` warns about it, and the next `ingest` prices it once a row exists.
- Duplicate transcript lines for the same API response are stored once, keeping the highest token counts.

## Changing prices

```sh
# a price change from a date: add a row (older calls keep the old price)
claudecost prices set --model claude-opus-5-5 --from 2026-12-01 \
  --input 4 --output 20 --cw5m 5 --cw1h 8 --cache-read 0.2 --source "pricing page 2026-12-01"

# bulk: export, edit, import
claudecost prices export > prices.json
claudecost prices import prices.json
```

Both reprice the affected calls immediately.

## Price update skill

`price_update_skill.md` is a Claude Code skill that checks the price table against Anthropic's live pricing page, shows proposed changes, and applies them after you confirm.

Install it for your user (all projects):

```sh
mkdir -p ~/.claude/skills/update-prices
cp price_update_skill.md ~/.claude/skills/update-prices/SKILL.md
```

Or for this project only, copy it to `.claude/skills/update-prices/SKILL.md` in the repo instead.

Then ask Claude Code to "check claudecost prices", or run `/update-prices`.
