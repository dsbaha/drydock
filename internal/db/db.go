// Package db owns the SQLite connection and schema migrations.
package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps a *sql.DB with small helpers.
type DB struct {
	*sql.DB
}

// Open creates the data directory, opens the SQLite database, and migrates it.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, "drydock.db")
	sqldb, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // SQLite + WAL: single writer keeps things simple
	d := &DB{sqldb}
	if err := d.migrate(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			token TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id)`,
		`CREATE TABLE IF NOT EXISTS boards (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cards (
			id TEXT PRIMARY KEY,
			board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			body TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'manual',
			column_name TEXT NOT NULL DEFAULT 'inbox',
			progress_pct INTEGER NOT NULL DEFAULT 0,
			container_id TEXT NOT NULL DEFAULT '',
			agent_runtime TEXT NOT NULL DEFAULT 'echo',
			has_open_question INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cards_board ON cards(board_id)`,
		`CREATE INDEX IF NOT EXISTS idx_cards_column ON cards(column_name)`,
		`CREATE TABLE IF NOT EXISTS steps (
			id TEXT PRIMARY KEY,
			card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			sort INTEGER NOT NULL DEFAULT 0,
			done INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_steps_card ON steps(card_id)`,
		`CREATE TABLE IF NOT EXISTS questions (
			id TEXT PRIMARY KEY,
			card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
			text TEXT NOT NULL,
			asker TEXT NOT NULL DEFAULT 'worker',
			answer TEXT NOT NULL DEFAULT '',
			answered_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_questions_card ON questions(card_id)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
			sender TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_card ON messages(card_id)`,
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY,
			card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_card ON events(card_id)`,
		`CREATE TABLE IF NOT EXISTS feeds (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			url TEXT NOT NULL UNIQUE,
			board_id TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			interval_sec INTEGER NOT NULL DEFAULT 900,
			last_polled_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS feed_items (
			feed_id TEXT NOT NULL REFERENCES feeds(id) ON DELETE CASCADE,
			guid TEXT NOT NULL,
			PRIMARY KEY (feed_id, guid)
		)`,
		`CREATE TABLE IF NOT EXISTS llm_providers (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			provider_type TEXT NOT NULL DEFAULT 'openai',
			endpoint TEXT NOT NULL DEFAULT '',
			api_key TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS webhook_source_filters (
			source TEXT PRIMARY KEY,
			filter_enabled INTEGER NOT NULL DEFAULT 0,
			filter_instructions TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS rejected_items (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			source TEXT NOT NULL,
			title TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			payload TEXT NOT NULL DEFAULT '',
			board_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rejected_created ON rejected_items(created_at)`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Additive migrations for databases created before these columns
	// existed. Idempotent: skipped when the column is already present.
	if err := d.ensureColumn("cards", "wait_reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := d.ensureColumn("feeds", "last_item_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := d.ensureColumn("feeds", "filter_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := d.ensureColumn("feeds", "filter_instructions", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := d.ensureColumn("cards", "transcript", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// ensureColumn adds column to table when it is missing (SQLite has no
// ADD COLUMN IF NOT EXISTS).
func (d *DB) ensureColumn(table, column, ddl string) error {
	var n int
	err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if n == 0 {
		if _, err := d.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, ddl)); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// NewID returns a random 32-hex-char identifier.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewToken returns a random 64-hex-char auth token.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Now returns the current Unix time in seconds.
func Now() int64 { return time.Now().Unix() }
