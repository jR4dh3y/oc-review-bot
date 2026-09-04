package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
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

// UpsertUser inserts or updates a GitHub user and returns it. When no admin
// exists yet (or forceAdmin is set, for the configured admin logins) the user
// becomes an admin.
func (s *Store) UpsertUser(githubID int64, login, avatarURL string, forceAdmin bool) (*User, error) {
	login = strings.ToLower(login)
	admin := forceAdmin
	if !admin {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1`).Scan(&count); err != nil {
			return nil, err
		}
		admin = count == 0
	}
	_, err := s.db.Exec(`INSERT INTO users (github_id, login, avatar_url, is_admin, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(github_id) DO UPDATE SET login = excluded.login, avatar_url = excluded.avatar_url`,
		githubID, login, avatarURL, admin, now())
	if err != nil {
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
	exp, _ := time.Parse(time.RFC3339, expires)
	if time.Now().UTC().After(exp) {
		s.DeleteSession(token)
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRow(
		`SELECT id, github_id, login, avatar_url, is_admin, created_at FROM users WHERE id = ?`, userID))
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}
