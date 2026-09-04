// Package store persists users, sessions, Zen keys, reviews, and settings
// in SQLite.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db     *sql.DB
	keyEnc KeyCipher
}

// KeyCipher encrypts Zen API keys at rest.
type KeyCipher interface {
	Seal(plaintext string) (string, error)
	Open(ciphertext string) (string, error)
}

// Open opens (creating if needed) the database at path and runs migrations.
// keyEnc encrypts API keys at rest.
func Open(path string, keyEnc KeyCipher) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, keyEnc: keyEnc}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
	id          INTEGER PRIMARY KEY,
	github_id   INTEGER NOT NULL UNIQUE,
	login       TEXT NOT NULL UNIQUE,
	avatar_url  TEXT NOT NULL DEFAULT '',
	is_admin    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token_hash  TEXT PRIMARY KEY,
	user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS zen_keys (
	id              INTEGER PRIMARY KEY,
	label           TEXT NOT NULL,
	ciphertext      TEXT NOT NULL,
	last4           TEXT NOT NULL,
	requests_today  INTEGER NOT NULL DEFAULT 0,
	usage_date      TEXT NOT NULL DEFAULT '',
	cooldown_until  TEXT NOT NULL DEFAULT '',
	disabled_at     TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS reviews (
	id                 INTEGER PRIMARY KEY,
	repo_full          TEXT NOT NULL,
	pr_number          INTEGER NOT NULL,
	head_sha           TEXT NOT NULL DEFAULT '',
	installation_id    INTEGER NOT NULL,
	requester_login    TEXT NOT NULL,
	trigger_comment_id INTEGER NOT NULL,
	status             TEXT NOT NULL DEFAULT 'queued',
	model              TEXT NOT NULL DEFAULT '',
	zen_key_id         INTEGER,
	summary_md         TEXT NOT NULL DEFAULT '',
	error              TEXT NOT NULL DEFAULT '',
	summary_comment_id INTEGER,
	created_at         TEXT NOT NULL,
	started_at         TEXT NOT NULL DEFAULT '',
	finished_at        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_reviews_repo_pr ON reviews(repo_full, pr_number);
CREATE TABLE IF NOT EXISTS findings (
	id                INTEGER PRIMARY KEY,
	review_id         INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
	path              TEXT NOT NULL,
	line              INTEGER NOT NULL,
	side              TEXT NOT NULL DEFAULT 'RIGHT',
	severity          TEXT NOT NULL DEFAULT 'info',
	body_md           TEXT NOT NULL,
	posted_comment_id INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS register_nudges (
	id         INTEGER PRIMARY KEY,
	repo_full  TEXT NOT NULL,
	pr_number  INTEGER NOT NULL,
	login      TEXT NOT NULL,
	UNIQUE(repo_full, pr_number, login)
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
