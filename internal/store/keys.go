package store

import (
	"database/sql"
	"errors"
	"time"
)

// ZenKey is a pooled OpenCode Zen API key. Plaintext never leaves this
// package; callers get the decrypted key via ZenKey.Secret.
type ZenKey struct {
	ID            int64
	Label         string
	Secret        string // decrypted, for runner use only
	Last4         string
	RequestsToday int
	CooldownUntil time.Time
	Disabled      bool
	CreatedAt     time.Time
}

// MaskedZenKey is the API-safe view of a key (no secret material).
type MaskedZenKey struct {
	ID            int64      `json:"id"`
	Label         string     `json:"label"`
	Last4         string     `json:"last4"`
	RequestsToday int        `json:"requests_today"`
	CooldownUntil *time.Time `json:"cooldown_until"`
	Disabled      bool       `json:"disabled"`
	CreatedAt     time.Time  `json:"created_at"`
}

// scanKey reads a row into a ZenKey with the plaintext secret decrypted.
func scanKey(row interface{ Scan(...any) error }, enc KeyCipher) (*ZenKey, error) {
	var k ZenKey
	var ct, cooldown, createdAt string
	var disabledAt sql.NullString
	err := row.Scan(&k.ID, &k.Label, &ct, &k.Last4, &k.RequestsToday, &cooldown, &disabledAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if enc != nil {
		if k.Secret, err = enc.Open(ct); err != nil {
			return nil, err
		}
	}
	k.CooldownUntil, _ = time.Parse(time.RFC3339, cooldown)
	k.Disabled = disabledAt.Valid && disabledAt.String != ""
	k.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &k, nil
}

const keyCols = `id, label, ciphertext, last4, requests_today, cooldown_until, disabled_at, created_at`

// AddKey stores a new encrypted Zen key.
func (s *Store) AddKey(label, secret string) (*ZenKey, error) {
	ct, err := s.keyEnc.Seal(secret)
	if err != nil {
		return nil, err
	}
	last4 := secret
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	res, err := s.db.Exec(`INSERT INTO zen_keys (label, ciphertext, last4, created_at) VALUES (?, ?, ?, ?)`,
		label, ct, last4, now())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Key(id)
}

func (s *Store) Key(id int64) (*ZenKey, error) {
	return scanKey(s.db.QueryRow(`SELECT `+keyCols+` FROM zen_keys WHERE id = ?`, id), s.keyEnc)
}

// AcquireKey atomically selects the least-used eligible key and records its
// use. Selection and increment must share one statement so concurrent workers
// cannot all choose the same key before any usage is recorded.
func (s *Store) AcquireKey() (*ZenKey, error) {
	return scanKey(s.db.QueryRow(`
UPDATE zen_keys
SET requests_today = `+todayUTC+` + 1,
    usage_date = strftime('%Y-%m-%d', 'now')
WHERE id = (
	SELECT id
	FROM zen_keys
	WHERE disabled_at = ''
	  AND (cooldown_until = '' OR cooldown_until <= ?)
	ORDER BY `+todayUTC+` ASC, id ASC
	LIMIT 1
)
RETURNING `+keyCols, now()), s.keyEnc)
}

// ListKeys returns masked keys newest-first for the admin UI.
func (s *Store) ListKeys() ([]MaskedZenKey, error) {
	rows, err := s.db.Query(`SELECT ` + keyCols + ` FROM zen_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MaskedZenKey
	for rows.Next() {
		k, err := scanKey(rows, nil)
		if err != nil {
			return nil, err
		}
		m := MaskedZenKey{
			ID: k.ID, Label: k.Label, Last4: k.Last4,
			RequestsToday: k.RequestsToday, Disabled: k.Disabled, CreatedAt: k.CreatedAt,
		}
		if !k.CooldownUntil.IsZero() {
			m.CooldownUntil = &k.CooldownUntil
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteKey removes a key.
func (s *Store) DeleteKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM zen_keys WHERE id = ?`, id)
	return err
}

// todayUTC is the requests-today counter, zeroing rows from earlier days.
const todayUTC = `(CASE WHEN usage_date = strftime('%Y-%m-%d','now') THEN requests_today ELSE 0 END)`

// PoolCandidates returns all enabled keys that are not cooling down, ordered
// by fewest requests today first.
func (s *Store) PoolCandidates() ([]ZenKey, error) {
	rows, err := s.db.Query(`SELECT `+keyCols+` FROM zen_keys
		WHERE disabled_at = ''
		  AND (cooldown_until = '' OR cooldown_until <= ?)
		ORDER BY `+todayUTC+` ASC, id ASC`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZenKey
	for rows.Next() {
		k, err := scanKey(rows, s.keyEnc)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// CountUsage returns the requests-today count for a key.
func (s *Store) CountUsage(id int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT `+todayUTC+` FROM zen_keys WHERE id = ?`, id).Scan(&n)
	return n, err
}

// RecordUsage bumps the requests-today counter for a key.
func (s *Store) RecordUsage(id int64) error {
	_, err := s.db.Exec(`UPDATE zen_keys
		SET requests_today = `+todayUTC+` + 1, usage_date = strftime('%Y-%m-%d','now')
		WHERE id = ?`, id)
	return err
}

// CoolKey puts a key on cooldown until t.
func (s *Store) CoolKey(id int64, until time.Time) error {
	_, err := s.db.Exec(`UPDATE zen_keys SET cooldown_until = ? WHERE id = ?`, until.UTC().Format(time.RFC3339), id)
	return err
}

// DisableKey marks a key as (not) disabled.
func (s *Store) DisableKey(id int64, disabled bool) error {
	if disabled {
		_, err := s.db.Exec(`UPDATE zen_keys SET disabled_at = ? WHERE id = ?`, now(), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE zen_keys SET disabled_at = '' WHERE id = ?`, id)
	return err
}

func (s *Store) GetSetting(key, fallback string) string {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return fallback
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
