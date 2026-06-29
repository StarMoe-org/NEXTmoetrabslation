package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite connection with the application schema applied.
type DB struct {
	*sql.DB
}

// Open opens (or creates) the SQLite database at path and applies the schema.
// modernc.org/sqlite is a pure-Go driver, so no CGO is required.
func Open(path string) (*DB, error) {
	// WAL lets many readers run concurrently with a single writer, which is the
	// whole point of using it here: background jobs (cn-sync, AI translate,
	// backup) hold long write transactions while editors keep reading. For that
	// concurrency to actually happen the pool must allow more than one
	// connection — capping it at 1 serializes every query behind the current
	// writer and turns sync windows into multi-minute stalls.
	//
	// _txlock=immediate makes write transactions begin with BEGIN IMMEDIATE so
	// they take the write lock up front and queue on busy_timeout instead of
	// deadlocking on lock upgrade (the classic multi-writer SQLITE_BUSY trap).
	// busy_timeout is raised to 10s to ride out the longest cn-sync commits.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_txlock=immediate", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Allow concurrent readers alongside the single writer (WAL's design). One
	// writer at a time is enforced by SQLite itself + BEGIN IMMEDIATE, not by
	// starving the pool.
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	d := &DB{sqlDB}
	if err := d.applySchema(); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return d, nil
}

func (d *DB) applySchema() error {
	_, err := d.Exec(schema)
	return err
}

const schema = `
CREATE TABLE IF NOT EXISTS entries (
	category    TEXT NOT NULL,
	field       TEXT NOT NULL,
	jp_key      TEXT NOT NULL,
	cn_text     TEXT NOT NULL DEFAULT '',
	source      TEXT NOT NULL DEFAULT 'unknown',
	ids_json    TEXT NOT NULL DEFAULT '',
	updated_at  INTEGER NOT NULL DEFAULT 0,
	updated_by  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (category, field, jp_key)
);
CREATE INDEX IF NOT EXISTS idx_entries_cat_field ON entries(category, field);
CREATE INDEX IF NOT EXISTS idx_entries_source ON entries(category, field, source);

CREATE TABLE IF NOT EXISTS event_stories (
	event_id     INTEGER PRIMARY KEY,
	source       TEXT NOT NULL DEFAULT 'unknown',
	version      TEXT NOT NULL DEFAULT '1.0',
	last_updated INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS event_story_episodes (
	event_id        INTEGER NOT NULL,
	episode_no      TEXT NOT NULL,
	scenario_id     TEXT NOT NULL DEFAULT '',
	title           TEXT NOT NULL DEFAULT '',
	title_source    TEXT NOT NULL DEFAULT '',
	talk_order_json TEXT NOT NULL DEFAULT '',
	position        INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (event_id, episode_no),
	FOREIGN KEY (event_id) REFERENCES event_stories(event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS event_story_lines (
	event_id     INTEGER NOT NULL,
	episode_no   TEXT NOT NULL,
	jp_key       TEXT NOT NULL,
	cn_text      TEXT NOT NULL DEFAULT '',
	source       TEXT NOT NULL DEFAULT 'unknown',
	speaker_name TEXT NOT NULL DEFAULT '',
	position     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (event_id, episode_no, jp_key),
	FOREIGN KEY (event_id) REFERENCES event_stories(event_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL DEFAULT 'editor',
	created_at    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS settings (
	key       TEXT PRIMARY KEY,
	value     TEXT NOT NULL DEFAULT '',
	encrypted INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS audit_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	ts     INTEGER NOT NULL,
	user   TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
`
