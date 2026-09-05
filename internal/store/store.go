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

var (
	ErrNotFound             = errors.New("not found")
	ErrActiveReview         = errors.New("an active review already exists")
	ErrReviewNotQueued      = errors.New("review is not queued")
	ErrReviewNotRunning     = errors.New("review is not running")
	ErrReviewLeaseLost      = errors.New("review execution lease was lost")
	ErrReviewNotReady       = errors.New("review publication is not complete")
	ErrPublicationReconcile = errors.New("publication requires reconciliation")
	ErrReviewReconcile      = errors.New("review requires reconciliation")
	ErrNudgeLeaseLost       = errors.New("nudge execution lease was lost")
	ErrNudgeReconcile       = errors.New("access notification requires reconciliation")
	ErrUnfencedFinding      = errors.New("findings must be written through the publication outbox")
	ErrServiceLeaseLost     = errors.New("service lease was lost")
	ErrServiceLeaseRequired = errors.New("service lease owner is required")
	ErrServiceLeaseFence    = errors.New("service lease fence is required")
)

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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
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
CREATE TABLE IF NOT EXISTS oauth_states (
	state_hash  TEXT PRIMARY KEY,
	client_hash TEXT NOT NULL DEFAULT '',
	expires_at  TEXT NOT NULL,
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_oauth_states_expires_at ON oauth_states(expires_at);
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
		repository_id      INTEGER NOT NULL DEFAULT 0,
		pr_number          INTEGER NOT NULL,
		head_sha           TEXT NOT NULL DEFAULT '',
		head_revision      TEXT NOT NULL DEFAULT '',
	installation_id    INTEGER NOT NULL,
	requester_github_id INTEGER NOT NULL DEFAULT 0,
	requester_login    TEXT NOT NULL,
	trigger_comment_id INTEGER NOT NULL,
	status             TEXT NOT NULL DEFAULT 'queued',
	model              TEXT NOT NULL DEFAULT '',
	zen_key_id         INTEGER,
	summary_md         TEXT NOT NULL DEFAULT '',
	error              TEXT NOT NULL DEFAULT '',
	summary_comment_id INTEGER,
		execution_generation INTEGER NOT NULL DEFAULT 0,
		service_lease_owner TEXT NOT NULL DEFAULT '',
		claim_fence        INTEGER NOT NULL DEFAULT 0,
		publication_token  TEXT NOT NULL DEFAULT '',
	publication_prepared INTEGER NOT NULL DEFAULT 0,
	next_attempt_at    TEXT NOT NULL DEFAULT '',
	created_at         TEXT NOT NULL,
	started_at         TEXT NOT NULL DEFAULT '',
	finished_at        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_reviews_repo_pr ON reviews(repo_full, pr_number);
CREATE INDEX IF NOT EXISTS idx_reviews_requester ON reviews(requester_github_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_reviews_queue ON reviews(status, created_at, id);
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
CREATE TABLE IF NOT EXISTS review_publications (
	id                INTEGER PRIMARY KEY,
	review_id         INTEGER NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
	kind              TEXT NOT NULL,
	ordinal           INTEGER NOT NULL,
	marker            TEXT NOT NULL,
	body_md           TEXT NOT NULL,
	commit_sha        TEXT NOT NULL DEFAULT '',
	path              TEXT NOT NULL DEFAULT '',
	side              TEXT NOT NULL DEFAULT '',
	line              INTEGER NOT NULL DEFAULT 0,
	finding_id        INTEGER REFERENCES findings(id) ON DELETE CASCADE,
	status            TEXT NOT NULL DEFAULT 'pending',
	posted_comment_id INTEGER NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	posted_at         TEXT NOT NULL DEFAULT '',
	service_lease_owner TEXT NOT NULL DEFAULT '',
	service_lease_fence INTEGER NOT NULL DEFAULT 0,
	send_started_at    TEXT NOT NULL DEFAULT '',
	UNIQUE(review_id, kind, ordinal),
	UNIQUE(marker)
);
CREATE INDEX IF NOT EXISTS idx_review_publications_pending ON review_publications(review_id, status, ordinal);
CREATE TABLE IF NOT EXISTS register_nudges (
	id         INTEGER PRIMARY KEY,
	repo_full  TEXT NOT NULL,
	pr_number  INTEGER NOT NULL,
	login      TEXT NOT NULL,
	UNIQUE(repo_full, pr_number, login)
);
CREATE TABLE IF NOT EXISTS access_nudges (
	id                   INTEGER PRIMARY KEY,
	repo_full            TEXT NOT NULL,
	repository_id        INTEGER NOT NULL,
	pr_number            INTEGER NOT NULL,
	installation_id      INTEGER NOT NULL,
	requester_github_id  INTEGER NOT NULL,
	requester_login      TEXT NOT NULL,
	kind                 TEXT NOT NULL,
	publication_token    TEXT NOT NULL,
		status               TEXT NOT NULL DEFAULT 'queued',
		execution_generation INTEGER NOT NULL DEFAULT 0,
		service_lease_owner TEXT NOT NULL DEFAULT '',
		claim_fence         INTEGER NOT NULL DEFAULT 0,
		posted_comment_id    INTEGER NOT NULL DEFAULT 0,
	next_attempt_at      TEXT NOT NULL DEFAULT '',
	created_at           TEXT NOT NULL,
	posted_at            TEXT NOT NULL DEFAULT '',
	send_started_at      TEXT NOT NULL DEFAULT '',
	UNIQUE(repository_id, pr_number, requester_github_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_access_nudges_queue ON access_nudges(status, next_attempt_at, id);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
	delivery_id TEXT PRIMARY KEY,
	created_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS service_leases (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	owner_token TEXT NOT NULL,
	fence       INTEGER NOT NULL DEFAULT 1,
	expires_at  TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	if err := ensureColumn(tx, "oauth_states", "client_hash", "client_hash TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_oauth_states_client_expires_at
		ON oauth_states(client_hash, expires_at)`); err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"repository_id", "repository_id INTEGER NOT NULL DEFAULT 0"},
		{"requester_github_id", "requester_github_id INTEGER NOT NULL DEFAULT 0"},
		{"execution_generation", "execution_generation INTEGER NOT NULL DEFAULT 0"},
		{"service_lease_owner", "service_lease_owner TEXT NOT NULL DEFAULT ''"},
		{"claim_fence", "claim_fence INTEGER NOT NULL DEFAULT 0"},
		{"publication_token", "publication_token TEXT NOT NULL DEFAULT ''"},
		{"publication_prepared", "publication_prepared INTEGER NOT NULL DEFAULT 0"},
		{"next_attempt_at", "next_attempt_at TEXT NOT NULL DEFAULT ''"},
		{"head_revision", "head_revision TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(tx, "reviews", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"service_lease_owner", "service_lease_owner TEXT NOT NULL DEFAULT ''"},
		{"claim_fence", "claim_fence INTEGER NOT NULL DEFAULT 0"},
		{"send_started_at", "send_started_at TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(tx, "access_nudges", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"service_lease_owner", "service_lease_owner TEXT NOT NULL DEFAULT ''"},
		{"service_lease_fence", "service_lease_fence INTEGER NOT NULL DEFAULT 0"},
		{"send_started_at", "send_started_at TEXT NOT NULL DEFAULT ''"},
	} {
		if err := ensureColumn(tx, "review_publications", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE service_leases SET fence = 1 WHERE fence < 1`); err != nil {
		return err
	}

	// A legacy row cannot be safely attributed to an immutable account or
	// repository. Do not run it after upgrading; a new signed mention creates a
	// fully authorized replacement.
	if _, err = tx.Exec(`UPDATE reviews
SET status = ?,
    error = CASE WHEN error = '' THEN ? ELSE error END,
    finished_at = CASE WHEN finished_at = '' THEN ? ELSE finished_at END
WHERE status IN (?, ?)
  AND (repository_id < 1 OR requester_github_id < 1 OR installation_id < 1 OR trigger_comment_id < 1)`,
		StatusFailed, "legacy review lacks immutable authorization identity; mention the bot again", now(),
		StatusQueued, StatusRunning); err != nil {
		return err
	}

	// Persisted administrator flags predate immutable allowlists. They are not
	// an authority source and must not survive an allowlist removal.
	if _, err = tx.Exec(`UPDATE users SET is_admin = 0 WHERE is_admin != 0`); err != nil {
		return err
	}

	// Older deployments may contain races from before the active-review
	// constraint existed. Keep the newest job and make stale jobs terminal.
	if _, err = tx.Exec(`
UPDATE reviews
SET status = ?,
    error = CASE WHEN error = '' THEN ? ELSE error END,
    finished_at = CASE WHEN finished_at = '' THEN ? ELSE finished_at END
WHERE status IN (?, ?)
  AND EXISTS (
			SELECT 1
			FROM reviews AS newer
			WHERE newer.repository_id = reviews.repository_id
			  AND newer.pr_number = reviews.pr_number
			  AND newer.status IN (?, ?, ?)
			  AND newer.id > reviews.id
		)`, StatusFailed, "superseded during active-review uniqueness migration", now(), StatusQueued, StatusRunning, StatusReconciliationRequired,
		StatusQueued, StatusRunning, StatusReconciliationRequired); err != nil {
		return err
	}
	if _, err = tx.Exec(`
UPDATE reviews
SET status = ?,
    error = CASE WHEN error = '' THEN ? ELSE error END,
    finished_at = CASE WHEN finished_at = '' THEN ? ELSE finished_at END
WHERE status = ?
  AND EXISTS (
			SELECT 1
			FROM reviews AS newer
			WHERE newer.repository_id = reviews.repository_id
			  AND newer.pr_number = reviews.pr_number
			  AND newer.status IN (?, ?, ?)
			  AND newer.id > reviews.id
		)`, StatusFailed, "superseded during active-review uniqueness migration", now(), StatusReconciliationRequired,
		StatusQueued, StatusRunning, StatusReconciliationRequired); err != nil {
		return err
	}
	if _, err = tx.Exec(`DROP INDEX IF EXISTS idx_reviews_one_active_per_pr`); err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_reviews_one_active_per_pr
			ON reviews(repository_id, pr_number)
			WHERE repository_id > 0 AND status IN ('queued', 'running', 'reconciliation_required')`); err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_reviews_one_trigger_event
		ON reviews(repository_id, trigger_comment_id)
		WHERE repository_id > 0 AND trigger_comment_id > 0`); err != nil {
		return err
	}

	return tx.Commit()
}

func ensureColumn(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + definition)
	return err
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
