---
name: update-prices
description: Check claudecost's price table against Anthropic's live pricing page and apply any changes. Use when asked to check, update, or refresh Claude model prices, when `claudecost report` warns about unpriced calls, or when a new Claude model appears.
---

# Update claudecost prices

claudecost stores prices in its SQLite database (`prices` and `settings` tables). Every API call's cost is calculated from the price row in effect at the call's timestamp. Applying a price change through the CLI reprices the affected calls automatically.

Run the CLI as `claudecost` if it is on PATH, otherwise `go run .` from the repo root. Pass `--db PATH` only if the user names a database.

## Rules

- Prices come only from the live pricing page fetched in this run. Never use remembered prices.
- Never invent a price for a model the page does not list. Report it as unpriced instead.
- Do not import anything until the user confirms the proposed changes.

## Steps

1. Collect the current state:
   - `claudecost prices export > <scratchpad>/prices-current.json`
   - `claudecost report --by model --days 0` (note any "no matching price" warning and which models it names)

2. Fetch `https://platform.claude.com/docs/en/about-claude/pricing.md` and extract, in USD per million tokens:
   - per model: base input, 5m cache write, 1h cache write, cache read ("cache hits and refreshes"), output
   - fast mode base rates. Cache rates for fast rows = fast base input x the page's cache multipliers for that model (5m write, 1h write, and that model's cache-read multiplier)
   - prompt-size tiers (e.g. Haiku 5.5 over 100,000 tokens). A tier row uses `min_prompt` = threshold + 1
   - settings: `geo_us_multiplier` (inference_geo "us"), `web_search_per_1k`
   - any stated effective date for a change

3. Compare the page to `prices-current.json`. Classify each difference:
   - **New model:** add a row with `effective_from` "2000-01-01".
   - **Correction** (the table was wrong): update the existing row and keep its `effective_from`.
   - **Price change on a date:** add a new row with `effective_from` = that date (YYYY-MM-DD, UTC). Keep the old row so earlier calls keep their old price.
   - If the page doesn't say whether a difference is a correction or a dated change, ask the user.

4. Show the user a table of proposed changes (model, speed, tier, field, old -> new, classification) and wait for confirmation.

5. Write only the changed or new rows to `<scratchpad>/prices-update.json` using the export format:
   ```json
   {"settings": {}, "prices": [{"model": "...", "speed": "standard", "min_prompt": 0, "effective_from": "2000-01-01",
     "input": 0, "output": 0, "cache_write_5m": 0, "cache_write_1h": 0, "cache_read": 0,
     "source": "platform.claude.com/docs/en/about-claude/pricing YYYY-MM-DD"}]}
   ```
   Include `settings` keys only if they changed.

6. Apply and verify:
   - `claudecost prices import <scratchpad>/prices-update.json`
   - `claudecost report --by model --days 0` and compare total cost with step 1
   - Report to the user: rows changed, calls repriced, total cost before -> after, and any models still unpriced.

If nothing differs, say so and stop.
