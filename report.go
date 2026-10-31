package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

var reportKeys = map[string]string{
	"day":     `date(ts, 'localtime')`,
	"model":   `model`,
	"effort":  `COALESCE(NULLIF(per_turn_effort, ''), NULLIF(effort, ''), '(none)')`,
	"project": `COALESCE(NULLIF(cwd, ''), '(unknown)')`,
	"session": `session_id`,
}

func cmdReport(db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	by := fs.String("by", "day", "day, model, effort, project or session")
	days := fs.Int("days", 30, "days to include, counting today (0 = all)")
	fs.Parse(args)

	key, ok := reportKeys[*by]
	if !ok {
		return fmt.Errorf("report: unknown --by %q", *by)
	}
	where := "1=1"
	if *days > 0 {
		where = fmt.Sprintf(`date(ts, 'localtime') > date('now', 'localtime', '-%d days')`, *days)
	}
	order := "cost DESC"
	if *by == "day" {
		order = "k"
	}
	q := fmt.Sprintf(`SELECT %s AS k, COUNT(*), COALESCE(SUM(cost_usd), 0) AS cost,
		SUM(input), SUM(output), SUM(thinking), SUM(cache_write_5m + cache_write_1h), SUM(cache_read),
		SUM(cost_usd IS NULL)
		FROM usage WHERE %s GROUP BY k ORDER BY %s`, key, where, order)
	rows, err := db.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(w, "%s\tcalls\tcost\tinput\toutput\tthinking\tcache_write\tcache_read\t\n", *by)
	var tCalls, tUnpriced int64
	var tCost float64
	for rows.Next() {
		var k string
		var calls, in, out, think, cw, cr, unpriced int64
		var cost float64
		if err := rows.Scan(&k, &calls, &cost, &in, &out, &think, &cw, &cr, &unpriced); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%d\t$%.2f\t%s\t%s\t%s\t%s\t%s\t\n", k, calls, cost, tok(in), tok(out), tok(think), tok(cw), tok(cr))
		tCalls += calls
		tCost += cost
		tUnpriced += unpriced
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(w, "total\t%d\t$%.2f\t\t\t\t\t\t\n", tCalls, tCost)
	w.Flush()
	if tUnpriced > 0 {
		fmt.Printf("warning: %d calls have no matching price (see: claudecost report --by model, claudecost prices list)\n", tUnpriced)
	}
	return nil
}

func tok(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
