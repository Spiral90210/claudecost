# claudecost

Go CLI that ingests Claude Code transcripts (`~/.claude/projects/**/*.jsonl`) into SQLite and reports cost. See README.md for usage.

- Build: `go build -o claudecost .`
- Test against a scratch database with `--db`, never the user's `~/.claude/cost.db`.
- Prices: never hardcode from memory. Defaults are in `prices.json`, sourced from the live pricing page.
- `price_update_skill.md` is a skill for checking and updating prices against the live pricing page. Install steps are in README.md.
