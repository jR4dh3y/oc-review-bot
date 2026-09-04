package store

import (
	"database/sql"
	"errors"
	"time"
)

// Review statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Review is one bot review of one PR.
type Review struct {
	ID               int64
	RepoFull         string
	PRNumber         int64
	HeadSHA          string
	InstallationID   int64
	RequesterLogin   string
	TriggerCommentID int64
	Status           string
	Model            string
	ZenKeyID         int64 // 0 = none
	SummaryMD        string
	Error            string
	SummaryCommentID int64
	CreatedAt        time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
}

// Finding is one inline review comment.
type Finding struct {
	ID              int64
	ReviewID        int64
	Path            string
	Line            int64
	Side            string // "RIGHT" (new) or "LEFT" (old)
	Severity        string
	BodyMD          string
	PostedCommentID int64
}

// NudgePosted reports whether we already told this login to register on this PR.
func (s *Store) NudgePosted(repoFull string, pr int64, login string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM register_nudges WHERE repo_full = ? AND pr_number = ? AND login = ?`,
		repoFull, pr, login).Scan(&n)
	return n > 0, err
}

// MarkNudge records that we nudged a login to register.
func (s *Store) MarkNudge(repoFull string, pr int64, login string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO register_nudges (repo_full, pr_number, login) VALUES (?, ?, ?)`,
		repoFull, pr, login)
	return err
}

// CreateReview inserts a queued review.
func (s *Store) CreateReview(r *Review) error {
	res, err := s.db.Exec(`INSERT INTO reviews
		(repo_full, pr_number, head_sha, installation_id, requester_login, trigger_comment_id, status, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RepoFull, r.PRNumber, r.HeadSHA, r.InstallationID, r.RequesterLogin, r.TriggerCommentID,
		StatusQueued, r.Model, now())
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

func scanReview(row interface{ Scan(...any) error }) (*Review, error) {
	var r Review
	var createdAt, startedAt, finishedAt string
	var zenKeyID, summaryCommentID sql.NullInt64
	err := row.Scan(&r.ID, &r.RepoFull, &r.PRNumber, &r.HeadSHA, &r.InstallationID, &r.RequesterLogin,
		&r.TriggerCommentID, &r.Status, &r.Model, &zenKeyID, &r.SummaryMD, &r.Error, &summaryCommentID,
		&createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ZenKeyID = zenKeyID.Int64
	r.SummaryCommentID = summaryCommentID.Int64
	r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	r.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	r.FinishedAt, _ = time.Parse(time.RFC3339, finishedAt)
	return &r, nil
}

const reviewCols = `id, repo_full, pr_number, head_sha, installation_id, requester_login, trigger_comment_id,
	status, model, zen_key_id, summary_md, error, summary_comment_id, created_at, started_at, finished_at`

func (s *Store) Review(id int64) (*Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE id = ?`, id))
}

// ReviewCounts returns (repo, pr) -> active review count map for dedupe.
func (s *Store) ActiveReview(repoFull string, pr int64) (*Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews
		WHERE repo_full = ? AND pr_number = ? AND status IN (?, ?) ORDER BY id DESC LIMIT 1`,
		repoFull, pr, StatusQueued, StatusRunning))
}

// StartReview marks a review running.
func (s *Store) StartReview(id int64, model string, keyID int64) error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, model = ?, zen_key_id = ?, started_at = ? WHERE id = ?`,
		StatusRunning, model, keyID, now(), id)
	return err
}

// FinishReviewDone stores the final summary and GitHub comment id.
func (s *Store) FinishReviewDone(id int64, summaryMD string, summaryCommentID int64) error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, summary_md = ?, summary_comment_id = ?, finished_at = ? WHERE id = ?`,
		StatusDone, summaryMD, summaryCommentID, now(), id)
	return err
}

// FinishReviewFailed stores the error.
func (s *Store) FinishReviewFailed(id int64, errText string) error {
	_, err := s.db.Exec(`UPDATE reviews SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		StatusFailed, errText, now(), id)
	return err
}

// SetHeadSHA stores the PR head commit for a review.
func (s *Store) SetHeadSHA(id int64, sha string) error {
	_, err := s.db.Exec(`UPDATE reviews SET head_sha = ? WHERE id = ?`, sha, id)
	return err
}

func (s *Store) ListReviews(limit int) ([]Review, error) {
	rows, err := s.db.Query(`SELECT `+reviewCols+` FROM reviews ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// SaveFinding inserts a finding and returns its id.
func (s *Store) SaveFinding(f *Finding) error {
	res, err := s.db.Exec(`INSERT INTO findings (review_id, path, line, side, severity, body_md, posted_comment_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, f.ReviewID, f.Path, f.Line, f.Side, f.Severity, f.BodyMD, f.PostedCommentID)
	if err != nil {
		return err
	}
	f.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) ListFindings(reviewID int64) ([]Finding, error) {
	rows, err := s.db.Query(`SELECT id, review_id, path, line, side, severity, body_md, posted_comment_id
		FROM findings WHERE review_id = ? ORDER BY id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if err := rows.Scan(&f.ID, &f.ReviewID, &f.Path, &f.Line, &f.Side, &f.Severity, &f.BodyMD, &f.PostedCommentID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
