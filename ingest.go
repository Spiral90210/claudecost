package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type record struct {
	Type          string `json:"type"`
	RequestID     string `json:"requestId"`
	Timestamp     string `json:"timestamp"`
	SessionID     string `json:"sessionId"`
	Cwd           string `json:"cwd"`
	GitBranch     string `json:"gitBranch"`
	IsSidechain   bool   `json:"isSidechain"`
	Effort        string `json:"effort"`
	PerTurnEffort string `json:"perTurnEffort"`
	Version       string `json:"version"`
	// type "continued-in"
	ContinuedIn string `json:"continuedInSessionId"`
	// type "cost-state": Claude Code's cumulative per-model totals for the session
	StartTime  int64 `json:"startTime"`
	ModelUsage map[string]struct {
		Input         int64   `json:"inputTokens"`
		Output        int64   `json:"outputTokens"`
		Thinking      int64   `json:"thinkingTokens"`
		CacheRead     int64   `json:"cacheReadInputTokens"`
		CacheCreation int64   `json:"cacheCreationInputTokens"`
		WebSearch     int64   `json:"webSearchRequests"`
		CostUSD       float64 `json:"costUSD"`
	} `json:"modelUsage"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			CacheCreation       int64 `json:"cache_creation_input_tokens"`
			CacheRead           int64 `json:"cache_read_input_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
			ServerToolUse struct {
				WebSearch int64 `json:"web_search_requests"`
				WebFetch  int64 `json:"web_fetch_requests"`
			} `json:"server_tool_use"`
			CacheCreationSplit *struct {
				E5m int64 `json:"ephemeral_5m_input_tokens"`
				E1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			ServiceTier  string `json:"service_tier"`
			Speed        string `json:"speed"`
			InferenceGeo string `json:"inference_geo"`
		} `json:"usage"`
	} `json:"message"`
}

type fileState struct {
	size, mtime, offset int64
}

type ingestStats struct {
	scanned, read, rewound, lines, upserts int
	bytes                                  int64
}

func cmdIngest(db *sql.DB, args []string) error {
	fset := flag.NewFlagSet("ingest", flag.ExitOnError)
	projects := fset.String("projects", "", "transcripts directory")
	quiet := fset.Bool("q", false, "no output")
	fset.Parse(args)

	start := time.Now()
	st, err := ingest(db, resolveProjects(*projects))
	if err != nil {
		return err
	}
	if !*quiet {
		fmt.Printf("ingest: %d files scanned, %d read, %s read, %d usage lines, %d rows upserted, %d rewound, %s\n",
			st.scanned, st.read, humanBytes(st.bytes), st.lines, st.upserts, st.rewound, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func ingest(db *sql.DB, root string) (ingestStats, error) {
	var st ingestStats
	// Offsets and prices are read inside the write transaction so a concurrent
	// ingest or price change that held the lock is fully seen.
	tx, err := db.Begin()
	if err != nil {
		return st, fmt.Errorf("ingest begin: %w", err)
	}
	defer tx.Rollback()
	pr, err := loadPricer(tx)
	if err != nil {
		return st, err
	}

	known := map[string]fileState{}
	rows, err := tx.Query(`SELECT path, size, mtime_ns, offset FROM files`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var p string
		var s fileState
		if err := rows.Scan(&p, &s.size, &s.mtime, &s.offset); err != nil {
			rows.Close()
			return st, err
		}
		known[p] = s
	}
	rows.Close()

	ins, err := tx.Prepare(`
		INSERT INTO usage (msg_id, request_id, ts, session_id, cwd, git_branch, is_subagent, model, effort, per_turn_effort,
			speed, service_tier, inference_geo, cc_version, input, output, thinking, cache_write_5m, cache_write_1h, cache_read,
			web_search, web_fetch, cost_usd, price_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (msg_id, request_id) DO UPDATE SET
			input = MAX(input, excluded.input), output = MAX(output, excluded.output),
			thinking = MAX(thinking, excluded.thinking),
			cache_write_5m = MAX(cache_write_5m, excluded.cache_write_5m),
			cache_write_1h = MAX(cache_write_1h, excluded.cache_write_1h),
			cache_read = MAX(cache_read, excluded.cache_read),
			web_search = MAX(web_search, excluded.web_search), web_fetch = MAX(web_fetch, excluded.web_fetch),
			cost_usd = NULL, price_id = NULL
		WHERE excluded.input > input OR excluded.output > output OR excluded.thinking > thinking
			OR excluded.cache_write_5m > cache_write_5m OR excluded.cache_write_1h > cache_write_1h
			OR excluded.cache_read > cache_read OR excluded.web_search > web_search OR excluded.web_fetch > web_fetch`)
	if err != nil {
		return st, err
	}
	defer ins.Close()

	upFile, err := tx.Prepare(`INSERT INTO files (path, size, mtime_ns, offset) VALUES (?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET size = excluded.size, mtime_ns = excluded.mtime_ns, offset = excluded.offset`)
	if err != nil {
		return st, err
	}
	defer upFile.Close()

	insApp, err := tx.Prepare(`INSERT OR IGNORE INTO appearances (msg_id, request_id, session_id) VALUES (?, ?, ?)`)
	if err != nil {
		return st, err
	}
	defer insApp.Close()

	insCont, err := tx.Prepare(`INSERT OR REPLACE INTO continuations (session_id, continued_in) VALUES (?, ?)`)
	if err != nil {
		return st, err
	}
	defer insCont.Close()

	// cost-state totals only grow, so keep the largest value seen for each field.
	insCS, err := tx.Prepare(`INSERT INTO cost_state (session_id, model, input, output, thinking, cache_creation, cache_read, web_search, cc_cost, start_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id, model) DO UPDATE SET
			input = MAX(input, excluded.input), output = MAX(output, excluded.output), thinking = MAX(thinking, excluded.thinking),
			cache_creation = MAX(cache_creation, excluded.cache_creation), cache_read = MAX(cache_read, excluded.cache_read),
			web_search = MAX(web_search, excluded.web_search), cc_cost = MAX(cc_cost, excluded.cc_cost)`)
	if err != nil {
		return st, err
	}
	defer insCS.Close()

	// WalkDir does not follow a symlinked root (e.g. ~/.claude/projects from dotfiles).
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		st.scanned++
		info, err := d.Info()
		if err != nil {
			return err
		}
		size, mtime := info.Size(), info.ModTime().UnixNano()
		prev, seen := known[path]
		if seen && prev.size == size && prev.mtime == mtime {
			return nil
		}
		offset := prev.offset
		if size < offset {
			offset = 0
			st.rewound++
		}
		subagent := strings.Contains(path, string(filepath.Separator)+"subagents"+string(filepath.Separator))
		consumed, err := readFrom(path, offset, func(line []byte) error {
			isUsage := bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"type":"assistant"`))
			if !isUsage && !bytes.Contains(line, []byte(`"type":"cost-state"`)) && !bytes.Contains(line, []byte(`"type":"continued-in"`)) {
				return nil
			}
			var r record
			if json.Unmarshal(line, &r) != nil {
				return nil
			}
			switch r.Type {
			case "cost-state":
				for m, u := range r.ModelUsage {
					if _, err := insCS.Exec(r.SessionID, normalizeModel(m), u.Input, u.Output, u.Thinking, u.CacheCreation,
						u.CacheRead, u.WebSearch, u.CostUSD, r.StartTime); err != nil {
						return err
					}
				}
				return nil
			case "continued-in":
				if r.SessionID != "" && r.ContinuedIn != "" {
					_, err := insCont.Exec(r.SessionID, r.ContinuedIn)
					return err
				}
				return nil
			case "assistant":
			default:
				return nil
			}
			if r.Message.Usage == nil || r.Message.ID == "" || r.Message.Model == "" || r.Message.Model == "<synthetic>" {
				return nil
			}
			st.lines++
			u := r.Message.Usage
			cw5, cw1 := u.CacheCreation, int64(0)
			if u.CacheCreationSplit != nil {
				cw5, cw1 = u.CacheCreationSplit.E5m, u.CacheCreationSplit.E1h
			}
			speed := u.Speed
			if speed == "" {
				speed = "standard"
			}
			c := call{Model: normalizeModel(r.Message.Model), Speed: speed, Geo: u.InferenceGeo, TS: r.Timestamp,
				In: u.InputTokens, Out: u.OutputTokens, CW5: cw5, CW1: cw1, CR: u.CacheRead, WebSearch: u.ServerToolUse.WebSearch}
			usd, pid, ok := pr.cost(c)
			res, err := ins.Exec(r.Message.ID, r.RequestID, r.Timestamp, r.SessionID, r.Cwd, r.GitBranch, subagent || r.IsSidechain,
				c.Model, r.Effort, r.PerTurnEffort, speed, u.ServiceTier, u.InferenceGeo, r.Version,
				c.In, c.Out, u.OutputTokensDetails.ThinkingTokens, cw5, cw1, c.CR, c.WebSearch, u.ServerToolUse.WebFetch,
				sql.NullFloat64{Float64: usd, Valid: ok}, sql.NullInt64{Int64: pid, Valid: ok})
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				st.upserts++
			}
			_, err = insApp.Exec(r.Message.ID, r.RequestID, r.SessionID)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		st.read++
		st.bytes += consumed
		_, err = upFile.Exec(path, size, mtime, offset+consumed)
		return err
	})
	if walkErr != nil {
		return st, walkErr
	}
	if st.read > 0 {
		if _, err := tx.Exec(ownerSQL); err != nil {
			return st, fmt.Errorf("session owners: %w", err)
		}
		if _, err := tx.Exec(unloggedSQL); err != nil {
			return st, fmt.Errorf("unlogged estimates: %w", err)
		}
	}
	// Rows whose tokens grew on a later duplicate line, new estimates, and models
	// that had no price yet.
	if _, err := repriceTx(tx, pr, `cost_usd IS NULL`); err != nil {
		return st, err
	}
	return st, tx.Commit()
}

// ownerSQL gives a message copied into a resumed session to the session it was
// first logged in: of the sessions it appears in, the one that is not the
// continuation of another.
const ownerSQL = `
UPDATE usage SET session_id = COALESCE((
	SELECT a.session_id FROM appearances a
	WHERE a.msg_id = usage.msg_id AND a.request_id = usage.request_id
		AND NOT EXISTS (
			SELECT 1 FROM appearances b JOIN continuations c ON c.session_id = b.session_id
			WHERE b.msg_id = a.msg_id AND b.request_id = a.request_id AND c.continued_in = a.session_id)
	ORDER BY a.session_id LIMIT 1), session_id)
WHERE estimated = 0 AND EXISTS (
	SELECT 1 FROM appearances a2
	WHERE a2.msg_id = usage.msg_id AND a2.request_id = usage.request_id AND a2.session_id <> usage.session_id)`

// unloggedSQL rebuilds one estimated row per session and model: Claude Code's
// cost-state totals minus every logged call that appears in that session's
// files. Copied calls count toward each session they appear in, because each
// session's cost-state includes them. Cache writes have no 5m/1h split in
// cost-state, so the remainder is priced at the 5m rate. The row is dated at the
// session's last logged call.
const unloggedSQL = `
DELETE FROM usage WHERE estimated = 1;
INSERT INTO usage (msg_id, request_id, ts, session_id, cwd, is_subagent, model, effort, speed,
	input, output, thinking, cache_write_5m, cache_write_1h, cache_read, web_search, web_fetch, estimated)
SELECT * FROM (
	SELECT 'unlogged:' || cs.session_id || ':' || cs.model, '',
		COALESCE(s.last_ts, strftime('%Y-%m-%dT%H:%M:%fZ', cs.start_ms / 1000.0, 'unixepoch')),
		cs.session_id, s.cwd, 0, cs.model, 'unlogged', 'standard',
		MAX(cs.input - COALESCE(o.input, 0), 0) AS gi,
		MAX(cs.output - COALESCE(o.output, 0), 0) AS go,
		MAX(cs.thinking - COALESCE(o.thinking, 0), 0),
		MAX(cs.cache_creation - COALESCE(o.cw, 0), 0) AS gcw,
		0,
		MAX(cs.cache_read - COALESCE(o.cr, 0), 0) AS gcr,
		MAX(cs.web_search - COALESCE(o.ws, 0), 0) AS gws,
		0, 1
	FROM cost_state cs
	LEFT JOIN (
		SELECT a.session_id, u.model, SUM(u.input) AS input, SUM(u.output) AS output, SUM(u.thinking) AS thinking,
			SUM(u.cache_write_5m + u.cache_write_1h) AS cw, SUM(u.cache_read) AS cr, SUM(u.web_search) AS ws
		FROM appearances a JOIN usage u ON u.msg_id = a.msg_id AND u.request_id = a.request_id AND u.estimated = 0
		GROUP BY a.session_id, u.model
	) o ON o.session_id = cs.session_id AND o.model = cs.model
	LEFT JOIN (
		SELECT a.session_id, MAX(u.ts) AS last_ts, MAX(u.cwd) AS cwd
		FROM appearances a JOIN usage u ON u.msg_id = a.msg_id AND u.request_id = a.request_id AND u.estimated = 0
		GROUP BY a.session_id
	) s ON s.session_id = cs.session_id
) WHERE gi + go + gcw + gcr + gws > 0`

// readFrom calls fn for each complete line after offset and returns the bytes consumed.
// A trailing line without a newline is still being written, so it is left for the next run.
func readFrom(path string, offset int64, fn func([]byte) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var consumed int64
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return consumed, nil
		}
		if err != nil {
			return consumed, err
		}
		consumed += int64(len(line))
		if err := fn(line); err != nil {
			return consumed, err
		}
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
