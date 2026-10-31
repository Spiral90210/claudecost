package main

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
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
`

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if err := seedPrices(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
