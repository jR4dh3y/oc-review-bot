package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxOAuthStates          = 1024
	maxOAuthStatesPerClient = 5
)

var (
	ErrOAuthStateLimit       = errors.New("too many active oauth states")
	ErrOAuthStateClientLimit = errors.New("too many active oauth states for client")
)

// User is a registered user of the website.
type User struct {
	ID        int64
	GitHubID  int64
	Login     string
	AvatarURL string
	IsAdmin   bool
	CreatedAt time.Time
}

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var createdAt string
	err := row.Scan(&u.ID, &u.GitHubID, &u.Login, &u.AvatarURL, &u.IsAdmin, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &u, nil
}

// UpsertUser inserts or updates a GitHub user and returns it. forceAdmin must
// come from trusted configuration; a first login never receives admin access.
func (s *Store) UpsertUser(githubID int64, login, avatarURL string, forceAdmin bool) (*User, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	if githubID < 1 || login == "" {
		return nil, errors.New("user requires an immutable GitHub ID and login")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// GitHub logins are mutable and may be reclaimed. Preserve the historic row
	// under a non-login label so a verified new GitHub ID can use its current login.
	if _, err := tx.Exec(`UPDATE users
		SET login = 'retired-' || github_id || '-' || lower(hex(randomblob(12)))
		WHERE lower(login) = ? AND github_id != ?`, login, githubID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO users (github_id, login, avatar_url, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(github_id) DO UPDATE SET
			login = excluded.login,
			avatar_url = excluded.avatar_url,
			is_admin = excluded.is_admin`,
		githubID, login, avatarURL, forceAdmin, now()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByGitHubID(githubID)
}

func (s *Store) UserByGitHubID(githubID int64) (*User, error) {
	return scanUser(s.db.QueryRow(
		`SELECT id, github_id, login, avatar_url, is_admin, created_at FROM users WHERE github_id = ?`, githubID))
}

func (s *Store) UserByLogin(login string) (*User, error) {
	return scanUser(s.db.QueryRow(
		`SELECT id, github_id, login, avatar_url, is_admin, created_at FROM users WHERE login = ?`, strings.ToLower(login)))
}

func (s *Store) SetUserAdmin(userID int64, admin bool) error {
	_, err := s.db.Exec(`UPDATE users SET is_admin = ? WHERE id = ?`, admin, userID)
	return err
}

// CreateSession stores a session token (hashed) valid for ttl.
func (s *Store) CreateSession(userID int64, token string, ttl time.Duration) error {
	if userID < 1 || token == "" || ttl <= 0 {
		return errors.New("session requires a user, token, and positive lifetime")
	}
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`,
		hashToken(token), userID, time.Now().UTC().Add(ttl).Format(time.RFC3339))
	return err
}

// SessionUser returns the user for a session token, if it is valid.
func (s *Store) SessionUser(token string) (*User, error) {
	var userID int64
	var expires string
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, hashToken(token)).
		Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return nil, fmt.Errorf("parse session expiration: %w", err)
	}
	if !time.Now().UTC().Before(exp) {
		if err := s.DeleteSession(token); err != nil {
			return nil, fmt.Errorf("delete expired session: %w", err)
		}
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRow(
		`SELECT id, github_id, login, avatar_url, is_admin, created_at FROM users WHERE id = ?`, userID))
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// DeleteAuthSessionAndOAuthState revokes the browser's session and any pending
// OAuth login attempt together, so logout cannot partially revoke credentials.
func (s *Store) DeleteAuthSessionAndOAuthState(sessionToken, oauthState string) error {
	if sessionToken == "" && oauthState == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if sessionToken != "" {
		if _, err := tx.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(sessionToken)); err != nil {
			return err
		}
	}
	if oauthState != "" {
		if _, err := tx.Exec(`DELETE FROM oauth_states WHERE state_hash = ?`, hashToken(oauthState)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CreateOAuthState records a short-lived, single-use OAuth state token. The
// raw value remains only in the browser's secure cookie.
func (s *Store) CreateOAuthState(state string, ttl time.Duration) error {
	return s.createOAuthState(state, "", ttl)
}

// CreateOAuthStateForClient bounds active OAuth attempts for one client. The
// client identifier is hashed before it is persisted with the state record.
func (s *Store) CreateOAuthStateForClient(state, client string, ttl time.Duration) error {
	client = strings.TrimSpace(client)
	if client == "" {
		return errors.New("oauth state requires a client identifier")
	}
	return s.createOAuthState(state, hashToken(client), ttl)
}

func (s *Store) createOAuthState(state, clientHash string, ttl time.Duration) error {
	if state == "" || ttl <= 0 {
		return errors.New("oauth state requires a token and positive lifetime")
	}
	nowTime := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM oauth_states WHERE expires_at <= ?`, nowTime.Format(time.RFC3339)); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE expires_at > ?`, nowTime.Format(time.RFC3339)).Scan(&active); err != nil {
		return err
	}
	if active >= maxOAuthStates {
		return ErrOAuthStateLimit
	}
	if clientHash != "" {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE client_hash = ? AND expires_at > ?`,
			clientHash, nowTime.Format(time.RFC3339)).Scan(&active); err != nil {
			return err
		}
		if active >= maxOAuthStatesPerClient {
			return ErrOAuthStateClientLimit
		}
	}
	if _, err := tx.Exec(`INSERT INTO oauth_states (state_hash, client_hash, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		hashToken(state), clientHash, nowTime.Add(ttl).Format(time.RFC3339), nowTime.Format(time.RFC3339)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ConsumeOAuthState atomically validates and deletes one OAuth state token.
// Concurrent callbacks carrying the same browser cookie can therefore succeed
// only once.
func (s *Store) ConsumeOAuthState(state string) (bool, error) {
	if state == "" {
		return false, nil
	}
	res, err := s.db.Exec(`DELETE FROM oauth_states WHERE state_hash = ? AND expires_at > ?`,
		hashToken(state), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows == 1, err
}

// DeleteOAuthState revokes a pending OAuth state without treating an already
// consumed or expired token as an error.
func (s *Store) DeleteOAuthState(state string) error {
	if state == "" {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM oauth_states WHERE state_hash = ?`, hashToken(state))
	return err
}
