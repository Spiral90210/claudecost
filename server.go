package main

import (
	"database/sql"
	_ "embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML string

type server struct {
	db       *sql.DB
	dbPath   string
	projects string
	tmpl     *template.Template

	mu       sync.Mutex
	lastRun  time.Time
	lastStat ingestStats
	lastDur  time.Duration
	lastErr  error
}

func cmdServe(db *sql.DB, dbPath string, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	interval := fs.Duration("interval", time.Minute, "ingest interval (0 = never)")
	projects := fs.String("projects", "", "transcripts directory")
	fs.Parse(args)

	tmpl, err := template.New("index").Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(indexHTML)
	if err != nil {
		return err
	}
	s := &server{db: db, dbPath: dbPath, projects: resolveProjects(*projects), tmpl: tmpl}
	s.runIngest()
	if *interval > 0 {
		go func() {
			for range time.Tick(*interval) {
				s.runIngest()
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/stamp", s.handleStamp)
	log.Printf("claudecost serving http://%s (db %s, ingest every %s)", *addr, dbPath, *interval)
	return http.ListenAndServe(*addr, mux)
}

func (s *server) runIngest() {
	start := time.Now()
	st, err := ingest(s.db, s.projects)
	s.mu.Lock()
	s.lastRun, s.lastStat, s.lastDur, s.lastErr = start, st, time.Since(start), err
	s.mu.Unlock()
	if err != nil {
		log.Printf("ingest: %v", err)
	}
}

func (s *server) handleStamp(w http.ResponseWriter, r *http.Request) {
	var n int64
	var cost float64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM usage`).Scan(&n, &cost)
	fmt.Fprintf(w, "%d:%.4f", n, cost)
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	rng, _ := strconv.Atoi(r.URL.Query().Get("range"))
	if rng != 7 && rng != 90 {
		rng = 30
	}
	p, err := s.buildPage(rng)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, p); err != nil {
		log.Printf("render: %v", err)
	}
}

// ---- view model ----

type kpi struct{ Label, Value, Sub, Tok string }
type rangeOpt struct {
	Label, Href string
	Active      bool
}
type tick struct {
	Label string
	Pos   float64
	Axis  bool
}
type seg struct {
	H     float64
	Class string
	Top   bool
}
type col struct {
	Segs     []seg
	Title    string
	XL       string
	Today    bool
	Selected bool
}
type nameVal struct {
	Name  string `json:"name"`
	Class string `json:"class"`
	V     string `json:"v"`
}
type dayInfo struct {
	Title string    `json:"title"`
	Total string    `json:"total"`
	Rows  []nameVal `json:"rows"`
	Toks  []nameVal `json:"toks"`
	Calls string    `json:"calls"`
}
type barRow struct {
	Name, Full, Class, Cost, Pct, Calls, Per, Tok string
	W                                             float64
}
type stat struct{ V, Label string }
type heatCell struct {
	Lvl int
	Tip string
}
type heatRow struct {
	Day   string
	Cells []heatCell
}
type sessRow struct {
	When, Project, Full, Model, Class, Effort, Calls, Sub, Dur, Tok, Out, Cost string
}

type page struct {
	DBPath, LastIngest, IngestNote, CallsTotal string
	Ranges                                     []rangeOpt
	KPIs                                       []kpi
	RangeLabel, RangeTotal                     string
	Legend                                     []nameVal
	Ticks                                      []tick
	Cols                                       []col
	BarGap                                     string
	VB, AvgPts                                 string
	Days                                       []dayInfo
	Readout                                    dayInfo
	Models, Efforts, Projects, Comp            []barRow
	Stats                                      []stat
	Heat                                       []heatRow
	HeatHours                                  []string
	Peak, Burn                                 string
	Sessions                                   []sessRow
	Unpriced                                   int64
	UnpricedModels                             string
	Stamp                                      string
	Empty                                      bool
}

// families fixes chart color and stacking order (bottom to top).
var families = []string{"opus", "sonnet", "haiku", "fable", "other"}

func family(model string) string {
	switch {
	case strings.Contains(model, "opus"):
		return "opus"
	case strings.Contains(model, "sonnet"):
		return "sonnet"
	case strings.Contains(model, "haiku"):
		return "haiku"
	case strings.Contains(model, "fable"), strings.Contains(model, "mythos"):
		return "fable"
	}
	return "other"
}

type totals struct {
	Cost                          float64
	Calls, In, Out, Think, CW, CR int64
}

func (t totals) tokens() int64 { return t.In + t.Out + t.CW + t.CR }

const totalsCols = `COALESCE(SUM(cost_usd), 0), COUNT(*), COALESCE(SUM(input), 0), COALESCE(SUM(output), 0),
	COALESCE(SUM(thinking), 0), COALESCE(SUM(cache_write_5m + cache_write_1h), 0), COALESCE(SUM(cache_read), 0)`

const localDay = `date(ts, 'localtime')`

func (s *server) sum(where string, args ...any) (totals, error) {
	var t totals
	err := s.db.QueryRow(`SELECT `+totalsCols+` FROM usage WHERE `+where, args...).
		Scan(&t.Cost, &t.Calls, &t.In, &t.Out, &t.Think, &t.CW, &t.CR)
	return t, err
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func (s *server) buildPage(rng int) (*page, error) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := today.AddDate(0, 0, -(rng - 1))
	inRange := localDay + ` >= ?`
	p := &page{DBPath: s.dbPath, BarGap: "3px"}

	s.mu.Lock()
	p.LastIngest = ago(now.Sub(s.lastRun))
	p.IngestNote = fmt.Sprintf("last ingest: %d of %d files read, +%s in %s", s.lastStat.read, s.lastStat.scanned, humanBytes(s.lastStat.bytes), s.lastDur.Round(time.Millisecond))
	if s.lastErr != nil {
		p.IngestNote = "last ingest failed: " + s.lastErr.Error()
	}
	s.mu.Unlock()

	for _, r := range []int{7, 30, 90} {
		p.Ranges = append(p.Ranges, rangeOpt{Label: fmt.Sprintf("%dd", r), Href: fmt.Sprintf("/?range=%d", r), Active: r == rng})
	}

	all, err := s.sum("1=1")
	if err != nil {
		return nil, err
	}
	p.CallsTotal = commas(all.Calls)
	var stampN int64
	var stampCost float64
	s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd), 0) FROM usage`).Scan(&stampN, &stampCost)
	p.Stamp = fmt.Sprintf("%d:%.4f", stampN, stampCost)
	if all.Calls == 0 {
		p.Empty = true
		return p, nil
	}

	// ---- KPIs ----
	tToday, err := s.sum(localDay+` = ?`, day(today))
	if err != nil {
		return nil, err
	}
	prev7, _ := s.sum(localDay+` >= ? AND `+localDay+` < ?`, day(today.AddDate(0, 0, -7)), day(today))
	last7, _ := s.sum(localDay+` >= ?`, day(today.AddDate(0, 0, -6)))
	prior7, _ := s.sum(localDay+` >= ? AND `+localDay+` < ?`, day(today.AddDate(0, 0, -13)), day(today.AddDate(0, 0, -6)))
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	mtd, _ := s.sum(localDay+` >= ?`, day(monthStart))
	lastMonth, _ := s.sum(localDay+` >= ? AND `+localDay+` < ?`, day(monthStart.AddDate(0, -1, 0)), day(monthStart))
	rangeT, err := s.sum(inRange, day(start))
	if err != nil {
		return nil, err
	}
	var activeDays int64
	s.db.QueryRow(`SELECT COUNT(DISTINCT `+localDay+`) FROM usage WHERE `+inRange, day(start)).Scan(&activeDays)

	tline := func(t totals) string {
		if t.Calls == 0 {
			return "no usage"
		}
		return fmt.Sprintf("%s tok | %s out | %s cache read", tok(t.tokens()), tok(t.Out), pct(float64(t.CR), float64(t.tokens())))
	}
	delta := "no prior data"
	if prior7.Cost > 0 {
		delta = fmt.Sprintf("%+.1f%% vs prior 7 days", (last7.Cost/prior7.Cost-1)*100)
	}
	avgActive := totals{}
	if activeDays > 0 {
		avgActive = totals{Cost: rangeT.Cost / float64(activeDays), Calls: 1,
			In: rangeT.In / activeDays, Out: rangeT.Out / activeDays, CW: rangeT.CW / activeDays, CR: rangeT.CR / activeDays}
	}
	p.KPIs = []kpi{
		{"Today", money(tToday.Cost), "so far | 7d avg " + money(prev7.Cost/7) + "/day", tline(tToday)},
		{"Last 7 days", money(last7.Cost), delta, tline(last7)},
		{monthStart.Format("January") + " (MTD)", money(mtd.Cost), monthStart.AddDate(0, -1, 0).Format("January") + " total " + money(lastMonth.Cost), tline(mtd)},
		{"Avg / active day", money(avgActive.Cost), fmt.Sprintf("%d of %d days active", activeDays, rng),
			fmt.Sprintf("%s tok | %s out per day", tok(avgActive.tokens()), tok(avgActive.Out))},
	}
	p.RangeLabel = fmt.Sprintf("last %d days", rng)
	p.RangeTotal = money(rangeT.Cost)

	// ---- daily chart ----
	type dayAgg struct {
		fam  map[string]float64
		tot  totals
		cw5  int64
		cw1  int64
		thnk int64
	}
	daily := map[string]*dayAgg{}
	rows, err := s.db.Query(`SELECT `+localDay+`, model, `+totalsCols+`, COALESCE(SUM(cache_write_5m), 0), COALESCE(SUM(cache_write_1h), 0)
		FROM usage WHERE `+inRange+` GROUP BY 1, 2`, day(start.AddDate(0, 0, -6)))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d, m string
		var t totals
		var cw5, cw1 int64
		if err := rows.Scan(&d, &m, &t.Cost, &t.Calls, &t.In, &t.Out, &t.Think, &t.CW, &t.CR, &cw5, &cw1); err != nil {
			rows.Close()
			return nil, err
		}
		a := daily[d]
		if a == nil {
			a = &dayAgg{fam: map[string]float64{}}
			daily[d] = a
		}
		a.fam[family(m)] += t.Cost
		a.tot.Cost += t.Cost
		a.tot.Calls += t.Calls
		a.tot.In += t.In
		a.tot.Out += t.Out
		a.tot.Think += t.Think
		a.tot.CW += t.CW
		a.tot.CR += t.CR
		a.cw5 += cw5
		a.cw1 += cw1
	}
	rows.Close()

	famTotals := map[string]float64{}
	maxDay := 0.0
	for i := 0; i < rng; i++ {
		if a := daily[day(start.AddDate(0, 0, i))]; a != nil {
			maxDay = math.Max(maxDay, a.tot.Cost)
			for f, c := range a.fam {
				famTotals[f] += c
			}
		}
	}
	step := niceStep(maxDay / 4)
	top := step * 4
	for k := 0; k <= 4; k++ {
		p.Ticks = append(p.Ticks, tick{Label: fmt.Sprintf("$%s", trimFloat(step*float64(k))), Pos: float64(k) * 25, Axis: k == 0})
	}
	for _, f := range families {
		if famTotals[f] > 0 {
			p.Legend = append(p.Legend, nameVal{Name: f, Class: "s-" + f, V: money(famTotals[f])})
		}
	}
	every := 7
	switch {
	case rng > 60:
		every, p.BarGap = 14, "1px"
	case rng <= 7:
		every, p.BarGap = 1, "12px"
	}
	var pts []string
	for i := 0; i < rng; i++ {
		d := start.AddDate(0, 0, i)
		a := daily[day(d)]
		if a == nil {
			a = &dayAgg{fam: map[string]float64{}}
		}
		c := col{Title: d.Format("Mon Jan 2") + ": " + money(a.tot.Cost), Today: i == rng-1, Selected: i == rng-1}
		if c.Today {
			c.XL = "today"
		} else if (rng-1-i)%every == 0 {
			c.XL = d.Format("Jan 2")
		}
		for j := len(families) - 1; j >= 0; j-- {
			f := families[j]
			if h := a.fam[f] / top * 100; h > 0.3 {
				c.Segs = append(c.Segs, seg{H: round2(h), Class: "s-" + f, Top: len(c.Segs) == 0})
			}
		}
		p.Cols = append(p.Cols, c)

		sum7 := 0.0
		for k := 0; k < 7; k++ {
			if b := daily[day(d.AddDate(0, 0, -k))]; b != nil {
				sum7 += b.tot.Cost
			}
		}
		pts = append(pts, fmt.Sprintf("%.2f,%.2f", float64(i)+0.5, 100-sum7/7/top*100))

		info := dayInfo{Title: d.Format("Mon Jan 2"), Total: money(a.tot.Cost), Calls: commas(a.tot.Calls) + " api calls | " + tok(a.tot.tokens()) + " tok"}
		if c.Today {
			info.Title += " (so far)"
		}
		for _, f := range families {
			if a.fam[f] > 0 {
				info.Rows = append(info.Rows, nameVal{Name: f, Class: "s-" + f, V: money(a.fam[f])})
			}
		}
		info.Toks = []nameVal{
			{Name: "input", V: tok(a.tot.In)},
			{Name: "output", V: tok(a.tot.Out) + " (" + pct(float64(a.tot.Think), float64(a.tot.Out)) + " think)"},
			{Name: "cache write 1h / 5m", V: tok(a.cw1) + " / " + tok(a.cw5)},
			{Name: "cache read", V: tok(a.tot.CR)},
		}
		p.Days = append(p.Days, info)
	}
	p.VB = fmt.Sprintf("0 0 %d 100", rng)
	p.AvgPts = strings.Join(pts, " ")
	p.Readout = p.Days[len(p.Days)-1]

	// ---- breakdowns ----
	p.Models, err = s.breakdown(`model`, day(start), rangeT.Cost, 0, func(k string) (string, string) { return k, "s-" + family(k) })
	if err != nil {
		return nil, err
	}
	p.Efforts, err = s.breakdown(reportKeys["effort"], day(start), rangeT.Cost, 0, nil)
	if err != nil {
		return nil, err
	}
	if p.Projects, err = s.projectBreakdown(day(start), rangeT.Cost, 5); err != nil {
		return nil, err
	}

	// ---- cost by token type ----
	settings, err := loadSettings(s.db)
	if err != nil {
		return nil, err
	}
	var cIn, cOut, cW5, cW1, cCR, cWS, saved float64
	var tIn, tOut, tThink, tW5, tW1, tCR, tWS int64
	err = s.db.QueryRow(`SELECT
		COALESCE(SUM(u.input * p.input * g), 0) / 1e6, COALESCE(SUM(u.output * p.output * g), 0) / 1e6,
		COALESCE(SUM(u.cache_write_5m * p.cache_write_5m * g), 0) / 1e6, COALESCE(SUM(u.cache_write_1h * p.cache_write_1h * g), 0) / 1e6,
		COALESCE(SUM(u.cache_read * p.cache_read * g), 0) / 1e6, COALESCE(SUM(u.web_search), 0) * ? / 1000,
		COALESCE(SUM(g * (u.cache_read * (p.input - p.cache_read) - u.cache_write_5m * (p.cache_write_5m - p.input)
			- u.cache_write_1h * (p.cache_write_1h - p.input))), 0) / 1e6,
		COALESCE(SUM(u.input), 0), COALESCE(SUM(u.output), 0), COALESCE(SUM(u.thinking), 0), COALESCE(SUM(u.cache_write_5m), 0),
		COALESCE(SUM(u.cache_write_1h), 0), COALESCE(SUM(u.cache_read), 0), COALESCE(SUM(u.web_search), 0)
		FROM (SELECT *, CASE WHEN inference_geo = 'us' THEN ? ELSE 1 END AS g FROM usage WHERE `+inRange+`) u
		JOIN prices p ON p.id = u.price_id`, settings["web_search_per_1k"], settings["geo_us_multiplier"], day(start)).
		Scan(&cIn, &cOut, &cW5, &cW1, &cCR, &cWS, &saved, &tIn, &tOut, &tThink, &tW5, &tW1, &tCR, &tWS)
	if err != nil {
		return nil, err
	}
	comp := []struct {
		name, tok string
		cost      float64
	}{
		{"output + thinking", tok(tOut) + " (" + tok(tThink) + " think)", cOut},
		{"cache write 1h", tok(tW1), cW1},
		{"cache read", tok(tCR), cCR},
		{"cache write 5m", tok(tW5), cW5},
		{"input (uncached)", tok(tIn), cIn},
	}
	if tWS > 0 {
		comp = append(comp, struct {
			name, tok string
			cost      float64
		}{"web search", commas(tWS) + " req", cWS})
	}
	sort.SliceStable(comp, func(i, j int) bool { return comp[i].cost > comp[j].cost })
	for _, c := range comp {
		p.Comp = append(p.Comp, barRow{Name: c.name, Tok: c.tok, Cost: money(c.cost), Pct: pct(c.cost, rangeT.Cost), W: share(c.cost, rangeT.Cost)})
	}
	var subCost float64
	s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd), 0) FROM usage WHERE is_subagent = 1 AND `+inRange, day(start)).Scan(&subCost)
	p.Stats = []stat{
		{tok(rangeT.tokens()), "tokens total"},
		{pct(float64(rangeT.CR), float64(rangeT.In+rangeT.CW+rangeT.CR)), "cache hit rate"},
		{money(saved), "net saved by caching"},
		{pct(subCost, rangeT.Cost), "spend in subagents"},
	}

	// ---- heatmap ----
	var heat [7][24]float64
	maxHeat := 0.0
	rows, err = s.db.Query(`SELECT CAST(strftime('%w', ts, 'localtime') AS INTEGER), CAST(strftime('%H', ts, 'localtime') AS INTEGER),
		COALESCE(SUM(cost_usd), 0) FROM usage WHERE `+inRange+` GROUP BY 1, 2`, day(start))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var wd, h int
		var c float64
		if err := rows.Scan(&wd, &h, &c); err != nil {
			rows.Close()
			return nil, err
		}
		r := (wd + 6) % 7
		heat[r][h] = c
		maxHeat = math.Max(maxHeat, c)
	}
	rows.Close()
	dayNames := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	peakR, peakH := 0, 0
	for r := 0; r < 7; r++ {
		hr := heatRow{Day: dayNames[r]}
		for h := 0; h < 24; h++ {
			v := heat[r][h]
			lvl := 0
			if v > 0 && maxHeat > 0 {
				lvl = int(math.Ceil(v / maxHeat * 5))
			}
			if v > heat[peakR][peakH] {
				peakR, peakH = r, h
			}
			hr.Cells = append(hr.Cells, heatCell{Lvl: lvl, Tip: fmt.Sprintf("%s %02d:00  %s", dayNames[r], h, money(v))})
		}
		p.Heat = append(p.Heat, hr)
	}
	p.HeatHours = []string{"00", "06", "12", "18"}
	p.Peak = fmt.Sprintf("%s %02d:00-%02d:00", dayNames[peakR], peakH, (peakH+1)%24)
	var activeHours int64
	s.db.QueryRow(`SELECT COUNT(DISTINCT strftime('%Y-%m-%d %H', ts, 'localtime')) FROM usage WHERE `+inRange, day(start)).Scan(&activeHours)
	if activeHours > 0 {
		p.Burn = money(rangeT.Cost/float64(activeHours)) + "/hr"
	}

	// ---- top sessions ----
	if p.Sessions, err = s.sessions(day(start)); err != nil {
		return nil, err
	}

	// ---- unpriced ----
	rows, err = s.db.Query(`SELECT model, COUNT(*) FROM usage WHERE cost_usd IS NULL GROUP BY model ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	var um []string
	for rows.Next() {
		var m string
		var n int64
		rows.Scan(&m, &n)
		p.Unpriced += n
		um = append(um, fmt.Sprintf("%s (%d)", m, n))
	}
	rows.Close()
	p.UnpricedModels = strings.Join(um, ", ")
	return p, nil
}

// breakdown groups range rows by key. limit > 0 folds the rest into "other".
func (s *server) breakdown(key, start string, total float64, limit int, label func(string) (string, string)) ([]barRow, error) {
	rows, err := s.db.Query(`SELECT `+key+` AS k, `+totalsCols+` FROM usage WHERE `+localDay+` >= ? GROUP BY k ORDER BY 2 DESC`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []barRow
	var other totals
	otherN := 0
	for rows.Next() {
		var k string
		var t totals
		if err := rows.Scan(&k, &t.Cost, &t.Calls, &t.In, &t.Out, &t.Think, &t.CW, &t.CR); err != nil {
			return nil, err
		}
		if limit > 0 && len(out) >= limit {
			other.Cost += t.Cost
			otherN++
			continue
		}
		name, class := k, ""
		if label != nil {
			name, class = label(k)
		}
		r := barRow{Name: name, Full: k, Class: class, Cost: money(t.Cost), Pct: pct(t.Cost, total), W: share(t.Cost, total),
			Calls: commas(t.Calls), Per: fmt.Sprintf("$%.3f", t.Cost/float64(t.Calls))}
		if label == nil {
			r.Tok = fmt.Sprintf("avg %s out/call | %s thinking", tok(t.Out/t.Calls), pct(float64(t.Think), float64(t.Out)))
		} else {
			r.Tok = fmt.Sprintf("in %s | out %s | cw %s | cr %s", tok(t.In), tok(t.Out), tok(t.CW), tok(t.CR))
		}
		out = append(out, r)
	}
	if otherN > 0 {
		out = append(out, barRow{Name: fmt.Sprintf("other (%d)", otherN), Cost: money(other.Cost), Pct: pct(other.Cost, total), W: share(other.Cost, total)})
	}
	return out, rows.Err()
}

// projectBreakdown groups cost by repo root (see projectRoot), top limit plus "other".
func (s *server) projectBreakdown(start string, total float64, limit int) ([]barRow, error) {
	rows, err := s.db.Query(`SELECT COALESCE(cwd, ''), COALESCE(SUM(cost_usd), 0) FROM usage WHERE `+localDay+` >= ? GROUP BY 1`, start)
	if err != nil {
		return nil, err
	}
	byRoot := map[string]float64{}
	for rows.Next() {
		var cwd string
		var c float64
		if err := rows.Scan(&cwd, &c); err != nil {
			rows.Close()
			return nil, err
		}
		byRoot[projectRoot(cwd)] += c
	}
	rows.Close()
	roots := make([]string, 0, len(byRoot))
	for r := range byRoot {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool { return byRoot[roots[i]] > byRoot[roots[j]] })
	var out []barRow
	other := 0.0
	for i, r := range roots {
		if i >= limit {
			other += byRoot[r]
			continue
		}
		out = append(out, barRow{Name: filepath.Base(r), Full: r, Cost: money(byRoot[r]), Pct: pct(byRoot[r], total), W: share(byRoot[r], total)})
	}
	if len(roots) > limit {
		out = append(out, barRow{Name: fmt.Sprintf("other (%d)", len(roots)-limit), Cost: money(other), Pct: pct(other, total), W: share(other, total)})
	}
	return out, nil
}

var rootCache sync.Map

// projectRoot maps a working directory to its repo: Claude Code worktrees
// (<repo>/.claude/worktrees/...) map to <repo>, and subdirectories map to the
// nearest ancestor containing .git. Paths that no longer exist are kept as is.
func projectRoot(cwd string) string {
	if cwd == "" {
		return "(unknown)"
	}
	if v, ok := rootCache.Load(cwd); ok {
		return v.(string)
	}
	p := cwd
	if i := strings.Index(p, string(filepath.Separator)+filepath.Join(".claude", "worktrees")+string(filepath.Separator)); i >= 0 {
		p = p[:i]
	}
	root := p
	for d := p; d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
	}
	rootCache.Store(cwd, root)
	return root
}

func (s *server) sessions(start string) ([]sessRow, error) {
	rows, err := s.db.Query(`SELECT session_id, MIN(ts), MAX(ts), MAX(cwd), COUNT(*), COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(CASE WHEN is_subagent = 1 THEN cost_usd END), 0),
		SUM(input + output + cache_write_5m + cache_write_1h + cache_read), SUM(output)
		FROM usage WHERE `+localDay+` >= ? GROUP BY session_id ORDER BY 6 DESC LIMIT 8`, start)
	if err != nil {
		return nil, err
	}
	type srow struct {
		id, first, last, cwd string
		calls, toks, out     int64
		cost, sub            float64
	}
	var list []srow
	for rows.Next() {
		var r srow
		if err := rows.Scan(&r.id, &r.first, &r.last, &r.cwd, &r.calls, &r.cost, &r.sub, &r.toks, &r.out); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()

	var out []sessRow
	for _, r := range list {
		var model, effort string
		s.db.QueryRow(`SELECT model FROM usage WHERE session_id = ? GROUP BY model ORDER BY SUM(cost_usd) DESC LIMIT 1`, r.id).Scan(&model)
		s.db.QueryRow(`SELECT `+reportKeys["effort"]+` AS e FROM usage WHERE session_id = ? GROUP BY e ORDER BY SUM(cost_usd) DESC LIMIT 1`, r.id).Scan(&effort)
		first, _ := time.Parse(time.RFC3339Nano, r.first)
		last, _ := time.Parse(time.RFC3339Nano, r.last)
		dur := last.Sub(first)
		out = append(out, sessRow{
			When: first.Local().Format("Jan 2 15:04"), Project: filepath.Base(projectRoot(r.cwd)), Full: r.cwd,
			Model: strings.TrimPrefix(model, "claude-"), Class: "s-" + family(model), Effort: effort,
			Calls: commas(r.calls), Sub: pct(r.sub, r.cost), Dur: fmt.Sprintf("%dh %02dm", int(dur.Hours()), int(dur.Minutes())%60),
			Tok: tok(r.toks), Out: tok(r.out), Cost: money(r.cost),
		})
	}
	return out, nil
}

// ---- formatting ----

func money(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	cents := int64(math.Round(v * 100))
	s := "$" + commas(cents/100) + fmt.Sprintf(".%02d", cents%100)
	if neg {
		s = "-" + s
	}
	return s
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func pct(a, b float64) string {
	if b == 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", a/b*100)
}

func share(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return round2(a / b * 100)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func niceStep(v float64) float64 {
	if v <= 0 {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*mag {
			return m * mag
		}
	}
	return 10 * mag
}

func trimFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh ago", int(d.Hours()))
}
