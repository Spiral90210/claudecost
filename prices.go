package main

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

//go:embed prices.json
var defaultPrices []byte

type Price struct {
	ID            int64   `json:"-"`
	Model         string  `json:"model"`
	Speed         string  `json:"speed"`
	MinPrompt     int64   `json:"min_prompt"`
	EffectiveFrom string  `json:"effective_from"`
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheWrite5m  float64 `json:"cache_write_5m"`
	CacheWrite1h  float64 `json:"cache_write_1h"`
	CacheRead     float64 `json:"cache_read"`
	Source        string  `json:"source"`
}

type PriceFile struct {
	Settings map[string]float64 `json:"settings"`
	Prices   []Price            `json:"prices"`
}

// call is the subset of a usage row needed to price it.
type call struct {
	Model, Speed, Geo, TS            string
	In, Out, CW5, CW1, CR, WebSearch int64
}

type pricer struct {
	rows      []Price
	geoUS     float64
	webSearch float64
}

func seedPrices(db *sql.DB) error {
	var pf PriceFile
	if err := json.Unmarshal(defaultPrices, &pf); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range pf.Settings {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM prices`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		for _, p := range pf.Prices {
			if err := upsertPrice(tx, p); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func upsertPrice(tx *sql.Tx, p Price) error {
	if p.Speed == "" {
		p.Speed = "standard"
	}
	_, err := tx.Exec(`
		INSERT INTO prices (model, speed, min_prompt, effective_from, input, output, cache_write_5m, cache_write_1h, cache_read, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (model, speed, min_prompt, effective_from) DO UPDATE SET
			input = excluded.input, output = excluded.output,
			cache_write_5m = excluded.cache_write_5m, cache_write_1h = excluded.cache_write_1h,
			cache_read = excluded.cache_read, source = excluded.source`,
		p.Model, p.Speed, p.MinPrompt, p.EffectiveFrom, p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead, p.Source)
	return err
}

func loadPrices(db *sql.DB) ([]Price, error) {
	rows, err := db.Query(`SELECT id, model, speed, min_prompt, effective_from, input, output, cache_write_5m, cache_write_1h, cache_read, COALESCE(source, '')
		FROM prices ORDER BY model, speed, min_prompt, effective_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Price
	for rows.Next() {
		var p Price
		if err := rows.Scan(&p.ID, &p.Model, &p.Speed, &p.MinPrompt, &p.EffectiveFrom, &p.Input, &p.Output, &p.CacheWrite5m, &p.CacheWrite1h, &p.CacheRead, &p.Source); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func loadSettings(db *sql.DB) (map[string]float64, error) {
	rows, err := db.Query(`SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func loadPricer(db *sql.DB) (*pricer, error) {
	rows, err := loadPrices(db)
	if err != nil {
		return nil, err
	}
	s, err := loadSettings(db)
	if err != nil {
		return nil, err
	}
	return &pricer{rows: rows, geoUS: s["geo_us_multiplier"], webSearch: s["web_search_per_1k"]}, nil
}

// cost picks the newest price row in effect at c.TS for the model and speed,
// using the highest prompt-size tier the request qualifies for.
func (p *pricer) cost(c call) (float64, int64, bool) {
	prompt := c.In + c.CW5 + c.CW1 + c.CR
	var best *Price
	for i := range p.rows {
		r := &p.rows[i]
		if r.Model != c.Model || r.Speed != c.Speed || r.MinPrompt > prompt || r.EffectiveFrom > c.TS {
			continue
		}
		if best == nil || r.EffectiveFrom > best.EffectiveFrom || (r.EffectiveFrom == best.EffectiveFrom && r.MinPrompt > best.MinPrompt) {
			best = r
		}
	}
	if best == nil {
		return 0, 0, false
	}
	usd := (float64(c.In)*best.Input + float64(c.Out)*best.Output + float64(c.CW5)*best.CacheWrite5m +
		float64(c.CW1)*best.CacheWrite1h + float64(c.CR)*best.CacheRead) / 1e6
	if c.Geo == "us" && p.geoUS > 0 {
		usd *= p.geoUS
	}
	usd += float64(c.WebSearch) * p.webSearch / 1000
	return usd, best.ID, true
}

func cmdPrices(db *sql.DB, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("prices: expected list, set, import or export")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("prices list", flag.ExitOnError)
		asJSON := fs.Bool("json", false, "print JSON")
		fs.Parse(args[1:])
		if *asJSON {
			return exportPrices(db)
		}
		ps, err := loadPrices(db)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(w, "id\tmodel\tspeed\tmin_prompt\tfrom\tinput\toutput\tcw_5m\tcw_1h\tcache_read\t")
		for _, p := range ps {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%g\t%g\t%g\t%g\t%g\t\n", p.ID, p.Model, p.Speed, p.MinPrompt, p.EffectiveFrom, p.Input, p.Output, p.CacheWrite5m, p.CacheWrite1h, p.CacheRead)
		}
		w.Flush()
		s, err := loadSettings(db)
		if err != nil {
			return err
		}
		for k, v := range s {
			fmt.Printf("setting %s = %g\n", k, v)
		}
		fmt.Println("prices are USD per million tokens")
		return nil

	case "set":
		fs := flag.NewFlagSet("prices set", flag.ExitOnError)
		var p Price
		fs.StringVar(&p.Model, "model", "", "model id (required)")
		fs.StringVar(&p.Speed, "speed", "standard", "standard or fast")
		fs.Int64Var(&p.MinPrompt, "min-prompt", 0, "tier applies when prompt tokens >= this")
		fs.StringVar(&p.EffectiveFrom, "from", "2000-01-01", "effective date YYYY-MM-DD (UTC)")
		fs.Float64Var(&p.Input, "input", -1, "USD per MTok")
		fs.Float64Var(&p.Output, "output", -1, "USD per MTok")
		fs.Float64Var(&p.CacheWrite5m, "cw5m", -1, "USD per MTok")
		fs.Float64Var(&p.CacheWrite1h, "cw1h", -1, "USD per MTok")
		fs.Float64Var(&p.CacheRead, "cache-read", -1, "USD per MTok")
		fs.StringVar(&p.Source, "source", "manual", "where the price came from")
		fs.Parse(args[1:])
		if p.Model == "" || p.Input < 0 || p.Output < 0 || p.CacheWrite5m < 0 || p.CacheWrite1h < 0 || p.CacheRead < 0 {
			return fmt.Errorf("prices set: --model, --input, --output, --cw5m, --cw1h and --cache-read are required")
		}
		return applyPrices(db, []Price{p}, nil)

	case "import":
		if len(args) < 2 {
			return fmt.Errorf("prices import: expected a JSON file")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var pf PriceFile
		if err := json.Unmarshal(b, &pf); err != nil {
			return err
		}
		return applyPrices(db, pf.Prices, pf.Settings)

	case "export":
		return exportPrices(db)
	}
	return fmt.Errorf("prices: unknown subcommand %q", args[0])
}

func applyPrices(db *sql.DB, ps []Price, settings map[string]float64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range ps {
		if err := upsertPrice(tx, p); err != nil {
			return err
		}
	}
	for k, v := range settings {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	where, wargs := "", []any{}
	if len(ps) == 1 && len(settings) == 0 {
		where, wargs = "model = ?", []any{ps[0].Model}
	}
	n, err := reprice(db, where, wargs...)
	if err != nil {
		return err
	}
	fmt.Printf("upserted %d price rows, repriced %d calls\n", len(ps), n)
	return nil
}

func exportPrices(db *sql.DB) error {
	ps, err := loadPrices(db)
	if err != nil {
		return err
	}
	s, err := loadSettings(db)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(PriceFile{Settings: s, Prices: ps})
}

func cmdReprice(db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("reprice", flag.ExitOnError)
	model := fs.String("model", "", "only reprice this model")
	fs.Parse(args)
	where, wargs := "", []any{}
	if *model != "" {
		where, wargs = "model = ?", []any{*model}
	}
	n, err := reprice(db, where, wargs...)
	if err != nil {
		return err
	}
	fmt.Printf("repriced %d calls\n", n)
	return nil
}

func reprice(db *sql.DB, where string, args ...any) (int, error) {
	pr, err := loadPricer(db)
	if err != nil {
		return 0, err
	}
	q := `SELECT id, model, speed, COALESCE(inference_geo, ''), ts, input, output, cache_write_5m, cache_write_1h, cache_read, web_search FROM usage`
	if where != "" {
		q += ` WHERE ` + where
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	type upd struct {
		id    int64
		cost  sql.NullFloat64
		price sql.NullInt64
	}
	var ups []upd
	for rows.Next() {
		var id int64
		var c call
		if err := rows.Scan(&id, &c.Model, &c.Speed, &c.Geo, &c.TS, &c.In, &c.Out, &c.CW5, &c.CW1, &c.CR, &c.WebSearch); err != nil {
			rows.Close()
			return 0, err
		}
		usd, pid, ok := pr.cost(c)
		ups = append(ups, upd{id, sql.NullFloat64{Float64: usd, Valid: ok}, sql.NullInt64{Int64: pid, Valid: ok}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE usage SET cost_usd = ?, price_id = ? WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, u := range ups {
		if _, err := stmt.Exec(u.cost, u.price, u.id); err != nil {
			return 0, err
		}
	}
	return len(ups), tx.Commit()
}
