package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func cmdSummary(db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	format := fs.String("format", "text", "text or swiftbar")
	noIngest := fs.Bool("no-ingest", false, "skip reading new transcript lines first")
	dash := fs.String("dashboard", "127.0.0.1:7777", "serve address to link to when it is running")
	projects := fs.String("projects", "", "transcripts directory")
	fs.Parse(args)

	var ingestErr error
	if !*noIngest {
		_, ingestErr = ingest(db, resolveProjects(*projects))
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	tToday, err := sumTotals(db, localDay+` = ?`, day(today))
	if err != nil {
		return err
	}
	prev7, _ := sumTotals(db, localDay+` >= ? AND `+localDay+` < ?`, day(today.AddDate(0, 0, -7)), day(today))
	last7, _ := sumTotals(db, localDay+` >= ?`, day(today.AddDate(0, 0, -6)))
	prior7, _ := sumTotals(db, localDay+` >= ? AND `+localDay+` < ?`, day(today.AddDate(0, 0, -13)), day(today.AddDate(0, 0, -6)))
	mtd, _ := sumTotals(db, localDay+` >= ?`, day(monthStart))
	var unpriced int64
	db.QueryRow(`SELECT COUNT(*) FROM usage WHERE cost_usd IS NULL`).Scan(&unpriced)
	var estToday float64
	db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE estimated = 1 AND `+localDay+` = ?`, day(today)).Scan(&estToday)

	byModel, err := todayBy(db, `model`, day(today))
	if err != nil {
		return err
	}
	byEffort, err := todayBy(db, reportKeys["effort"], day(today))
	if err != nil {
		return err
	}

	delta := ""
	if prior7.Cost > 0 {
		delta = fmt.Sprintf("(%+.1f%%)", (last7.Cost/prior7.Cost-1)*100)
	}
	lines := [][2]string{
		{"Today", fmt.Sprintf("%-9s (7d avg %s)", money(tToday.Cost), money(prev7.Cost/7))},
		{"Last 7 days", fmt.Sprintf("%-9s %s", money(last7.Cost), delta)},
		{monthStart.Format("January") + " MTD", money(mtd.Cost)},
	}
	tokLine := "Tokens today: none"
	if tToday.Calls > 0 {
		tokLine = fmt.Sprintf("Tokens today: %s (%s out, %s cache read), %s calls",
			tok(tToday.tokens()), tok(tToday.Out), pct(float64(tToday.CR), float64(tToday.tokens())), commas(tToday.Calls))
	}
	if estToday > 0 {
		tokLine += fmt.Sprintf("; today includes %s estimated for unlogged calls", money(estToday))
	}

	if *format != "swiftbar" {
		for _, l := range lines {
			fmt.Printf("%-14s %s\n", l[0], strings.TrimRight(l[1], " "))
		}
		printGroup("By model (today)", byModel, "  ", "")
		printGroup("By effort (today)", byEffort, "  ", "")
		fmt.Println(tokLine)
		if unpriced > 0 {
			fmt.Printf("warning: %d calls have no price\n", unpriced)
		}
		if ingestErr != nil {
			fmt.Println("ingest failed:", ingestErr)
		}
		return nil
	}

	// SwiftBar: lines before the first --- are the menu bar title; "|" starts a line's options.
	const mono = " | font=Menlo size=12"
	title := money(tToday.Cost) + " | sfimage=dollarsign.circle"
	if unpriced > 0 || ingestErr != nil {
		title = money(tToday.Cost) + " | sfimage=exclamationmark.triangle"
	}
	fmt.Println(title)
	fmt.Println("---")
	for _, l := range lines {
		fmt.Printf("%-14s %s%s\n", l[0], strings.TrimRight(l[1], " "), mono)
	}
	fmt.Println("---")
	printGroup("By model (today)", byModel, "--", mono)
	printGroup("By effort (today)", byEffort, "--", mono)
	fmt.Println(tokLine + " | size=11")
	if unpriced > 0 {
		fmt.Printf("%d calls have no price, run the price update skill | color=#d03b3b sfimage=exclamationmark.triangle\n", unpriced)
	}
	if ingestErr != nil {
		fmt.Printf("Ingest failed: %s | color=#d03b3b\n", strings.ReplaceAll(ingestErr.Error(), "|", "/"))
	}
	fmt.Println("---")
	if conn, err := net.DialTimeout("tcp", *dash, 200*time.Millisecond); err == nil {
		conn.Close()
		fmt.Printf("Open dashboard | href=http://%s sfimage=chart.bar\n", *dash)
	} else {
		fmt.Println("Dashboard not running (claudecost serve) | sfimage=chart.bar")
	}
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("Ingest now | bash=%q param1=ingest terminal=false refresh=true sfimage=arrow.clockwise\n", exe)
	}
	return nil
}

type keyCost struct {
	Key  string
	Cost float64
}

func todayBy(db *sql.DB, key, today string) ([]keyCost, error) {
	rows, err := db.Query(`SELECT `+key+` AS k, COALESCE(SUM(cost_usd), 0) AS c FROM usage WHERE `+localDay+` = ? GROUP BY k ORDER BY c DESC`, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keyCost
	for rows.Next() {
		var kc keyCost
		if err := rows.Scan(&kc.Key, &kc.Cost); err != nil {
			return nil, err
		}
		kc.Key = strings.TrimPrefix(kc.Key, "claude-")
		out = append(out, kc)
	}
	return out, rows.Err()
}

func printGroup(header string, items []keyCost, indent, opts string) {
	if len(items) == 0 {
		return
	}
	fmt.Println(header)
	width := 0
	for _, it := range items {
		width = max(width, len(it.Key))
	}
	for _, it := range items {
		fmt.Printf("%s%-*s  %s%s\n", indent, width, it.Key, money(it.Cost), opts)
	}
}
