package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
	path     TEXT PRIMARY KEY,
	size     INTEGER NOT NULL,
	mtime_ns INTEGER NOT NULL,
	offset   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS usage (
	id              INTEGER PRIMARY KEY,
	msg_id          TEXT NOT NULL,
	request_id      TEXT NOT NULL,
	ts              TEXT NOT NULL,
	session_id      TEXT,
	cwd             TEXT,
	git_branch      TEXT,
	is_subagent     INTEGER NOT NULL DEFAULT 0,
	model           TEXT NOT NULL,
	effort          TEXT,
	per_turn_effort TEXT,
	speed           TEXT NOT NULL DEFAULT 'standard',
	service_tier    TEXT,
	inference_geo   TEXT,
	cc_version      TEXT,
	input           INTEGER NOT NULL DEFAULT 0,
	output          INTEGER NOT NULL DEFAULT 0,
	thinking        INTEGER NOT NULL DEFAULT 0,
	cache_write_5m  INTEGER NOT NULL DEFAULT 0,
	cache_write_1h  INTEGER NOT NULL DEFAULT 0,
	cache_read      INTEGER NOT NULL DEFAULT 0,
	web_search      INTEGER NOT NULL DEFAULT 0,
	web_fetch       INTEGER NOT NULL DEFAULT 0,
	cost_usd        REAL,
	price_id        INTEGER,
	UNIQUE (msg_id, request_id)
);
CREATE INDEX IF NOT EXISTS usage_ts ON usage (ts);
CREATE INDEX IF NOT EXISTS usage_model ON usage (model);

CREATE TABLE IF NOT EXISTS prices (
	id             INTEGER PRIMARY KEY,
	model          TEXT NOT NULL,
	speed          TEXT NOT NULL DEFAULT 'standard',
	min_prompt     INTEGER NOT NULL DEFAULT 0,
	effective_from TEXT NOT NULL,
	input          REAL NOT NULL,
	output         REAL NOT NULL,
	cache_write_5m REAL NOT NULL,
	cache_write_1h REAL NOT NULL,
	cache_read     REAL NOT NULL,
	source         TEXT,
	UNIQUE (model, speed, min_prompt, effective_from)
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value REAL NOT NULL
);

-- Every session a message was logged in. Resumed sessions copy earlier lines,
-- so one message can appear in several sessions.
CREATE TABLE IF NOT EXISTS appearances (
	msg_id     TEXT NOT NULL,
	request_id TEXT NOT NULL,
	session_id TEXT NOT NULL,
	PRIMARY KEY (msg_id, request_id, session_id)
);
CREATE INDEX IF NOT EXISTS appearances_session ON appearances (session_id);

-- "continued-in" records: session_id continued as continued_in.
CREATE TABLE IF NOT EXISTS continuations (
	session_id   TEXT PRIMARY KEY,
	continued_in TEXT NOT NULL
);

-- Claude Code's own cumulative per-session, per-model totals ("cost-state" records).
CREATE TABLE IF NOT EXISTS cost_state (
	session_id     TEXT NOT NULL,
	model          TEXT NOT NULL,
	input          INTEGER NOT NULL,
	output         INTEGER NOT NULL,
	thinking       INTEGER NOT NULL,
	cache_creation INTEGER NOT NULL,
	cache_read     INTEGER NOT NULL,
	web_search     INTEGER NOT NULL,
	cc_cost        REAL NOT NULL,
	start_ms       INTEGER NOT NULL,
	PRIMARY KEY (session_id, model)
);
`

// migrations run once each, in order, tracked by PRAGMA user_version.
var migrations = []string{
	// 1: unlogged-call estimates. Re-read every transcript to fill appearances,
	// continuations and cost_state, and strip date suffixes from model ids.
	`ALTER TABLE usage ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0;
	UPDATE files SET size = -1, mtime_ns = 0, offset = 0;
	UPDATE usage SET model = substr(model, 1, length(model) - 9), cost_usd = NULL, price_id = NULL
		WHERE model GLOB '*-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]';`,
}

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// immediate: concurrent ingests (serve + menu bar) queue for the write lock instead of failing.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// Two processes creating a new database at once can get SQLITE_BUSY without the
	// busy timeout applying (the file is not in WAL mode yet), so retry setup briefly.
	for attempt := 0; ; attempt++ {
		err = setupDB(db)
		var se *sqlite.Error
		if err == nil || attempt >= 100 || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func setupDB(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := seedPrices(db); err != nil {
		return fmt.Errorf("seed prices: %w", err)
	}
	return nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= len(migrations) {
		return nil
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}
