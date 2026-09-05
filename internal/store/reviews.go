package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Review statuses.
const (
	StatusQueued                 = "queued"
	StatusRunning                = "running"
	StatusDone                   = "done"
	StatusFailed                 = "failed"
	StatusReconciliationRequired = "reconciliation_required"
)

const (
	PublicationSummary                = "summary"
	PublicationInline                 = "inline"
	PublicationPending                = "pending"
	PublicationSending                = "sending"
	PublicationPosted                 = "posted"
	PublicationReconciliationRequired = "reconciliation_required"

	NudgeRegistration           = "registration"
	NudgeEntitlement            = "entitlement"
	NudgeQueued                 = "queued"
	NudgePosting                = "posting"
	NudgePosted                 = "posted"
	NudgeFailed                 = "failed"
	NudgeReconciliationRequired = "reconciliation_required"
)

const reconciliationRetryDelay = 5 * time.Minute

// Review is one bot review of one PR.
type Review struct {
	ID                  int64
	RepoFull            string
	RepositoryID        int64
	PRNumber            int64
	HeadSHA             string
	HeadRevision        string
	InstallationID      int64
	RequesterGitHubID   int64
	RequesterLogin      string
	TriggerCommentID    int64
	Status              string
	Model               string
	ZenKeyID            int64 // 0 = none
	SummaryMD           string
	Error               string
	SummaryCommentID    int64
	ExecutionGeneration int64
	ServiceLeaseOwner   string
	ClaimFence          int64
	PublicationToken    string
	PublicationPrepared bool
	NextAttemptAt       time.Time
	CreatedAt           time.Time
	StartedAt           time.Time
	FinishedAt          time.Time
}

// Finding is one normalized inline review finding. It is persisted before a
// matching GitHub publication is attempted.
type Finding struct {
	ID                int64
	ReviewID          int64
	Path              string
	Line              int64
	Side              string // "RIGHT" (new) or "LEFT" (old)
	Severity          string
	BodyMD            string
	PostedCommentID   int64
	ServiceLeaseOwner string
	ServiceLeaseFence int64
	SendingStartedAt  time.Time
}

// Publication is one exact, durable GitHub-side effect for a review.
type Publication struct {
	ID                int64
	ReviewID          int64
	Kind              string
	Ordinal           int
	Marker            string
	BodyMD            string
	CommitSHA         string
	Path              string
	Side              string
	Line              int64
	FindingID         int64
	Status            string
	PostedCommentID   int64
	ServiceLeaseOwner string
	ServiceLeaseFence int64
	SendingStartedAt  time.Time
}

// PublicationFinding carries both the dashboard finding and its pre-rendered
// GitHub comment body. The store appends an opaque reconciliation marker.
type PublicationFinding struct {
	Path          string
	Line          int64
	Side          string
	Severity      string
	FindingBodyMD string
	CommentBodyMD string
}

// PublicationPlan is written transactionally before any result comments.
type PublicationPlan struct {
	SummaryMD     string
	SummaryBodyMD string
	CommitSHA     string
	Findings      []PublicationFinding
}

// ReviewAdmission applies configured service-wide paid-capacity limits.
type ReviewAdmission struct {
	UserPerHour int
	RepoPerHour int
	MaxActive   int
}

// ReviewCreateResult describes whether a webhook created a review or was a
// replay of a delivery that was already handled.
type ReviewCreateResult struct {
	Created                bool
	DuplicateDelivery      bool
	RateLimited            bool
	QueueFull              bool
	ReconciliationRequired bool
}

// Nudge is a durable access-notification outbox record.
type Nudge struct {
	ID                     int64
	RepoFull               string
	RepositoryID           int64
	PRNumber               int64
	InstallationID         int64
	RequesterGitHubID      int64
	RequesterLogin         string
	Kind                   string
	PublicationToken       string
	Status                 string
	ExecutionGeneration    int64
	ServiceLeaseOwner      string
	ClaimFence             int64
	PostedCommentID        int64
	CreatedAt              time.Time
	PostedAt               time.Time
	NextAttemptAt          time.Time
	SendingStartedAt       time.Time
	ReconciliationRequired bool
}

// NudgeCreateResult describes a durable nudge intake decision.
type NudgeCreateResult struct {
	Created           bool
	DuplicateDelivery bool
}

// CreateReview inserts a queued review unless an active review already exists.
// Production webhook intake uses CreateReviewWithAdmission.
func (s *Store) CreateReview(r *Review) error {
	result, err := s.CreateReviewOnce(r, "")
	if err != nil {
		return err
	}
	if !result.Created {
		return ErrActiveReview
	}
	return nil
}

// CreateReviewOnce persists a webhook delivery and queues its review in one
// transaction. It applies no capacity limits and is intended for migrations
// and focused tests; webhook intake must use CreateReviewWithAdmission.
func (s *Store) CreateReviewOnce(r *Review, deliveryID string) (ReviewCreateResult, error) {
	return s.CreateReviewWithAdmission(r, deliveryID, ReviewAdmission{})
}

// CreateReviewWithAdmission atomically records a delivery, enforces capacity
// limits, and creates a review. A trigger comment can create only one review,
// even when GitHub redelivers it with a different delivery ID.
func (s *Store) CreateReviewWithAdmission(r *Review, deliveryID string, admission ReviewAdmission) (ReviewCreateResult, error) {
	if err := validateReviewIdentity(r); err != nil {
		return ReviewCreateResult{}, err
	}
	token, err := newOpaqueToken()
	if err != nil {
		return ReviewCreateResult{}, fmt.Errorf("generate publication token: %w", err)
	}
	deliveryID = strings.TrimSpace(deliveryID)
	r.RequesterLogin = strings.ToLower(strings.TrimSpace(r.RequesterLogin))

	tx, err := s.db.Begin()
	if err != nil {
		return ReviewCreateResult{}, err
	}
	defer tx.Rollback()

	createdAt := now()
	if deliveryID != "" {
		created, err := claimDelivery(tx, deliveryID, createdAt)
		if err != nil {
			return ReviewCreateResult{}, err
		}
		if !created {
			if err := tx.Commit(); err != nil {
				return ReviewCreateResult{}, err
			}
			return ReviewCreateResult{DuplicateDelivery: true}, nil
		}
	}

	if admission.UserPerHour > 0 {
		count, err := countReviewsSince(tx, `requester_github_id = ?`, r.RequesterGitHubID)
		if err != nil {
			return ReviewCreateResult{}, err
		}
		if count >= admission.UserPerHour {
			if err := tx.Commit(); err != nil {
				return ReviewCreateResult{}, err
			}
			return ReviewCreateResult{RateLimited: true}, nil
		}
	}
	if admission.RepoPerHour > 0 {
		count, err := countReviewsSince(tx, `repository_id = ?`, r.RepositoryID)
		if err != nil {
			return ReviewCreateResult{}, err
		}
		if count >= admission.RepoPerHour {
			if err := tx.Commit(); err != nil {
				return ReviewCreateResult{}, err
			}
			return ReviewCreateResult{RateLimited: true}, nil
		}
	}
	if admission.MaxActive > 0 {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM reviews WHERE status IN (?, ?, ?)`, StatusQueued, StatusRunning, StatusReconciliationRequired).Scan(&count); err != nil {
			return ReviewCreateResult{}, err
		}
		if count >= admission.MaxActive {
			if err := tx.Commit(); err != nil {
				return ReviewCreateResult{}, err
			}
			return ReviewCreateResult{QueueFull: true}, nil
		}
	}

	res, err := tx.Exec(`INSERT INTO reviews
		(repo_full, repository_id, pr_number, head_sha, installation_id, requester_github_id, requester_login,
		 trigger_comment_id, status, model, publication_token, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		r.RepoFull, r.RepositoryID, r.PRNumber, r.HeadSHA, r.InstallationID, r.RequesterGitHubID, r.RequesterLogin,
		r.TriggerCommentID, StatusQueued, r.Model, token, createdAt)
	if err != nil {
		return ReviewCreateResult{}, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return ReviewCreateResult{}, err
	}
	if rows == 0 {
		var status string
		if err := tx.QueryRow(`SELECT status FROM reviews WHERE repository_id = ? AND pr_number = ?
				AND status IN (?, ?, ?) ORDER BY id DESC LIMIT 1`, r.RepositoryID, r.PRNumber,
			StatusQueued, StatusRunning, StatusReconciliationRequired).Scan(&status); err == nil && status == StatusReconciliationRequired {
			if err := tx.Commit(); err != nil {
				return ReviewCreateResult{}, err
			}
			return ReviewCreateResult{ReconciliationRequired: true}, nil
		}
		if err := tx.Commit(); err != nil {
			return ReviewCreateResult{}, err
		}
		return ReviewCreateResult{}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return ReviewCreateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewCreateResult{}, err
	}
	r.ID = id
	r.PublicationToken = token
	return ReviewCreateResult{Created: true}, nil
}

func validateReviewIdentity(r *Review) error {
	if r == nil || r.RepoFull == "" || r.RepositoryID < 1 || r.PRNumber < 1 || r.InstallationID < 1 ||
		r.RequesterGitHubID < 1 || r.RequesterLogin == "" || r.TriggerCommentID < 1 {
		return errors.New("review requires immutable repository, installation, requester, and comment identities")
	}
	return nil
}

func claimDelivery(tx *sql.Tx, deliveryID, createdAt string) (bool, error) {
	res, err := tx.Exec(`INSERT INTO webhook_deliveries (delivery_id, created_at)
		VALUES (?, ?) ON CONFLICT(delivery_id) DO NOTHING`, deliveryID, createdAt)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	return rows > 0, err
}

func countReviewsSince(tx *sql.Tx, where string, value int64) (int, error) {
	var count int
	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	err := tx.QueryRow(`SELECT COUNT(*) FROM reviews WHERE `+where+` AND created_at >= ?`, value, cutoff).Scan(&count)
	return count, err
}

func newOpaqueToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func scanReview(row interface{ Scan(...any) error }) (*Review, error) {
	var r Review
	var createdAt, startedAt, finishedAt, nextAttemptAt string
	var zenKeyID, summaryCommentID sql.NullInt64
	var publicationPrepared int
	err := row.Scan(&r.ID, &r.RepoFull, &r.RepositoryID, &r.PRNumber, &r.HeadSHA, &r.HeadRevision, &r.InstallationID,
		&r.RequesterGitHubID, &r.RequesterLogin, &r.TriggerCommentID, &r.Status, &r.Model, &zenKeyID,
		&r.SummaryMD, &r.Error, &summaryCommentID, &r.ExecutionGeneration, &r.ServiceLeaseOwner, &r.ClaimFence, &r.PublicationToken,
		&publicationPrepared, &nextAttemptAt, &createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.ZenKeyID = zenKeyID.Int64
	r.SummaryCommentID = summaryCommentID.Int64
	r.PublicationPrepared = publicationPrepared != 0
	r.NextAttemptAt, _ = time.Parse(time.RFC3339, nextAttemptAt)
	r.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	r.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	r.FinishedAt, _ = time.Parse(time.RFC3339, finishedAt)
	return &r, nil
}

const reviewCols = `id, repo_full, repository_id, pr_number, head_sha, head_revision, installation_id, requester_github_id,
		requester_login, trigger_comment_id, status, model, zen_key_id, summary_md, error, summary_comment_id,
		execution_generation, service_lease_owner, claim_fence, publication_token, publication_prepared, next_attempt_at, created_at, started_at, finished_at`

func (s *Store) Review(id int64) (*Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE id = ?`, id))
}

// ActiveReview returns the queued, running, or unresolved review for an
// immutable repository. An unresolved publication must block a replacement
// review until its remote effect is reconciled.
func (s *Store) ActiveReview(repositoryID, pr int64) (*Review, error) {
	return scanReview(s.db.QueryRow(`SELECT `+reviewCols+` FROM reviews
			WHERE repository_id = ? AND pr_number = ? AND status IN (?, ?, ?) ORDER BY id DESC LIMIT 1`,
		repositoryID, pr, StatusQueued, StatusRunning, StatusReconciliationRequired))
}

// ClaimQueuedReview is intentionally unavailable without a service lease term.
// Keeping an old owner-less claim path would let a stale process create work
// that the singleton lease is meant to fence.
func (s *Store) ClaimQueuedReview(id int64) (*Review, bool, error) {
	return nil, false, ErrServiceLeaseRequired
}

// ClaimReviewByID atomically creates a new execution generation for one queued
// row while recording the current service lease term.
func (s *Store) ClaimReviewByID(id int64, owner string, fence int64) (*Review, bool, error) {
	for attempt := 0; attempt < 6; attempt++ {
		review, claimed, err := s.claimReviewByIDOnce(id, owner, fence)
		if err == nil || !isSQLiteBusy(err) || attempt == 5 {
			return review, claimed, err
		}
		time.Sleep(time.Duration(attempt+1) * 20 * time.Millisecond)
	}
	return nil, false, errors.New("claim review retry exhausted")
}

func (s *Store) claimReviewByIDOnce(id int64, owner string, fence int64) (*Review, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return nil, false, err
	}
	r, claimed, err := claimReview(tx, id, owner, fence)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return r, claimed, nil
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "sqlite_busy")
}

// ClaimNextQueuedReview claims the oldest durable queue entry only while owner
// holds the live singleton service lease. The lease check and row transition
// share one write transaction so an expired worker cannot claim after handoff.
func (s *Store) ClaimNextQueuedReview(owner string, fence int64) (*Review, error) {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return nil, ErrServiceLeaseRequired
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return nil, err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM reviews
				WHERE status IN (?, ?) AND (next_attempt_at = '' OR next_attempt_at <= ?)
					AND EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)
					ORDER BY created_at, id LIMIT 1`, StatusQueued, StatusReconciliationRequired, now(), owner, fence, serviceLeaseTime(time.Now().UTC())).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if leaseErr := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); leaseErr != nil {
				return nil, leaseErr
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	r, claimed, err := claimReview(tx, id, owner, fence)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if leaseErr := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); leaseErr != nil {
			return nil, leaseErr
		}
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

func claimReview(tx *sql.Tx, id int64, owner string, fence int64) (*Review, bool, error) {
	res, err := tx.Exec(`UPDATE reviews
					SET status = ?, started_at = ?, next_attempt_at = '', service_lease_owner = ?, claim_fence = ?, execution_generation = execution_generation + 1
						WHERE id = ? AND status IN (?, ?) AND
						EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)`,
		StatusRunning, now(), owner, fence, id, StatusQueued, StatusReconciliationRequired, owner, fence, serviceLeaseTime(time.Now().UTC()))
	if err != nil {
		return nil, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if rows == 0 {
		return nil, false, nil
	}
	r, err := scanReview(tx.QueryRow(`SELECT `+reviewCols+` FROM reviews WHERE id = ?`, id))
	return r, true, err
}

// RecoverInterruptedReviews is called by the current lease owner. It only
// requeues rows from an older fencing term and leaves a recent outbound send
// alone until its bounded HTTP handoff window has elapsed.
func (s *Store) RecoverInterruptedReviews(owner string, fence int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE reviews SET status = ?, started_at = '', next_attempt_at = '', service_lease_owner = ''
			WHERE status = ? AND claim_fence < ? AND
				(publication_prepared = 0 OR NOT EXISTS (
					SELECT 1 FROM review_publications
					WHERE review_id = reviews.id AND status = ? AND send_started_at > ?
				))`, StatusQueued, StatusRunning, fence, PublicationSending, leaseDeadline(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count), nil
}

// RecoverQueuedReviews is unavailable without a service lease term. Engines
// must use RecoverInterruptedReviews before starting workers.
func (s *Store) RecoverQueuedReviews() ([]int64, error) {
	return nil, ErrServiceLeaseRequired
}

// StartReview is a compatibility helper for tests and administrative repair.
// It always creates a new fenced generation before recording its execution.
func (s *Store) StartReview(id int64, model string, keyID int64, owner string, fence int64) error {
	r, claimed, err := s.ClaimReviewByID(id, owner, fence)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrReviewNotQueued
	}
	return s.SetReviewExecution(id, r.ExecutionGeneration, model, keyID, owner, fence)
}

// SetReviewExecution records the model and key for a current worker only.
func (s *Store) SetReviewExecution(id, generation int64, model string, keyID int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET model = ?, zen_key_id = ?
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		model, keyID, id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// SetHeadSHA stores the PR head commit for a current worker only.
func (s *Store) SetHeadSHA(id, generation int64, sha string, owner string, fence int64) error {
	return s.SetReviewRevision(id, generation, "", sha, "", owner, fence)
}

// SetReviewRevision persists the canonical repository name, commit, and the
// provider's mutable PR revision token together. The revision token detects a
// force-push that returns to an earlier SHA (ABA) while this review is running.
func (s *Store) SetReviewRevision(id, generation int64, repoFull, sha, revision, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET repo_full = CASE WHEN ? <> '' THEN ? ELSE repo_full END,
			head_sha = ?, head_revision = ?
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		repoFull, repoFull, sha, revision, id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// SetNudgeRepository persists the verified canonical owner/name before a
// notification can be retried after a repository rename.
func (s *Store) SetNudgeRepository(id, generation int64, repoFull, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET repo_full = ?
		WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		repoFull, id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// ReviewLeaseCurrent reports whether this worker still owns the review.
func (s *Store) ReviewLeaseCurrent(id, generation int64, owner string, fence int64) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reviews
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?
			  AND EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)`,
		id, StatusRunning, generation, owner, fence, owner, fence, serviceLeaseTime(time.Now().UTC())).Scan(&count)
	return count == 1, err
}

func reviewLeaseCurrentTx(tx *sql.Tx, id, generation int64, owner string, fence int64) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM reviews
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?
			  AND EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)`,
		id, StatusRunning, generation, owner, fence, owner, fence, serviceLeaseTime(time.Now().UTC())).Scan(&count)
	return count == 1, err
}

func requireReviewLeaseTx(tx *sql.Tx, id, generation int64, owner string, fence int64) error {
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return err
	}
	current, err := reviewLeaseCurrentTx(tx, id, generation, owner, fence)
	if err != nil {
		return err
	}
	if !current {
		return ErrReviewLeaseLost
	}
	return nil
}

func requireReviewMutation(res sql.Result) error {
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrReviewLeaseLost
	}
	return nil
}

// PrepareReviewPublication persists a complete result/comment outbox before
// any GitHub write. An interrupted review can later publish this exact plan.
func (s *Store) PrepareReviewPublication(id, generation int64, plan PublicationPlan, owner string, fence int64) error {
	if plan.SummaryBodyMD == "" || plan.CommitSHA == "" {
		return errors.New("publication requires a summary body and commit SHA")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}

	var token string
	var prepared int
	err = tx.QueryRow(`SELECT publication_token, publication_prepared FROM reviews
		WHERE id = ? AND status = ? AND execution_generation = ?`, id, StatusRunning, generation).Scan(&token, &prepared)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrReviewLeaseLost
	}
	if err != nil {
		return err
	}
	if prepared != 0 || token == "" {
		return ErrReviewNotReady
	}

	summaryToken, err := newOpaqueToken()
	if err != nil {
		return fmt.Errorf("generate summary marker: %w", err)
	}
	summaryMarker := publicationMarker(summaryToken)
	if _, err := tx.Exec(`INSERT INTO review_publications
		(review_id, kind, ordinal, marker, body_md, commit_sha, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, PublicationSummary, 0, summaryMarker,
		withMarker(plan.SummaryBodyMD, summaryMarker), plan.CommitSHA, now()); err != nil {
		return err
	}
	for ordinal, finding := range plan.Findings {
		if finding.Path == "" || finding.Line < 1 || finding.Side == "" || finding.CommentBodyMD == "" {
			return errors.New("publication contains an invalid inline finding")
		}
		findingResult, err := tx.Exec(`INSERT INTO findings
			(review_id, path, line, side, severity, body_md, posted_comment_id)
			VALUES (?, ?, ?, ?, ?, ?, 0)`, id, finding.Path, finding.Line, finding.Side,
			finding.Severity, finding.FindingBodyMD)
		if err != nil {
			return err
		}
		findingID, err := findingResult.LastInsertId()
		if err != nil {
			return err
		}
		markerToken, err := newOpaqueToken()
		if err != nil {
			return fmt.Errorf("generate inline marker: %w", err)
		}
		marker := publicationMarker(markerToken)
		if _, err := tx.Exec(`INSERT INTO review_publications
			(review_id, kind, ordinal, marker, body_md, commit_sha, path, side, line, finding_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, PublicationInline, ordinal+1, marker,
			withMarker(finding.CommentBodyMD, marker), plan.CommitSHA, finding.Path, finding.Side, finding.Line,
			findingID, now()); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE reviews SET summary_md = ?, publication_prepared = 1
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		plan.SummaryMD, id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

func publicationMarker(token string) string {
	return fmt.Sprintf("<!-- oc-review-bot:v2:%s -->", token)
}

func withMarker(body, marker string) string {
	return strings.TrimSpace(body) + "\n\n" + marker
}

// ListReviewPublications returns the exact persisted GitHub effects in posting
// order. Inline comments go first so the summary acts as the completion signal.
func (s *Store) ListReviewPublications(reviewID int64) ([]Publication, error) {
	rows, err := s.db.Query(`SELECT id, review_id, kind, ordinal, marker, body_md, commit_sha, path, side, line,
			COALESCE(finding_id, 0), status, posted_comment_id, service_lease_owner, service_lease_fence, send_started_at
		FROM review_publications WHERE review_id = ?
		ORDER BY CASE kind WHEN 'inline' THEN 0 ELSE 1 END, ordinal`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var publications []Publication
	for rows.Next() {
		var p Publication
		var sendingStartedAt string
		if err := rows.Scan(&p.ID, &p.ReviewID, &p.Kind, &p.Ordinal, &p.Marker, &p.BodyMD, &p.CommitSHA,
			&p.Path, &p.Side, &p.Line, &p.FindingID, &p.Status, &p.PostedCommentID, &p.ServiceLeaseOwner,
			&p.ServiceLeaseFence, &sendingStartedAt); err != nil {
			return nil, err
		}
		p.SendingStartedAt, _ = time.Parse(time.RFC3339, sendingStartedAt)
		publications = append(publications, p)
	}
	return publications, rows.Err()
}

// ClaimPublicationForSend claims the durable publication for preflight reads.
// It deliberately leaves a pending publication pending: only
// BeginPublicationSend may record that the outbound POST is about to start.
// This keeps provider read failures retryable and distinguishable from an
// ambiguous remote write.
func (s *Store) ClaimPublicationForSend(reviewID, generation, publicationID int64, owner string, fence int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, reviewID, generation, owner, fence); err != nil {
		return false, err
	}
	var status, startedAt, publicationOwner string
	var publicationFence int64
	err = tx.QueryRow(`SELECT status, send_started_at, service_lease_owner, service_lease_fence
			FROM review_publications WHERE id = ? AND review_id = ?`, publicationID, reviewID).
		Scan(&status, &startedAt, &publicationOwner, &publicationFence)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if status == PublicationPosted {
		return false, tx.Commit()
	}
	if status == PublicationReconciliationRequired {
		return false, errors.Join(ErrPublicationReconcile, tx.Commit())
	}
	if status == PublicationPending && publicationOwner == owner && publicationFence == fence {
		return false, tx.Commit()
	}
	nowTime := time.Now().UTC()
	if status == PublicationSending {
		started, parseErr := time.Parse(time.RFC3339, startedAt)
		if parseErr != nil {
			if _, updateErr := tx.Exec(`UPDATE review_publications SET status = ?, service_lease_owner = '', service_lease_fence = 0
				WHERE id = ? AND review_id = ? AND status = ?`, PublicationReconciliationRequired, publicationID, reviewID, PublicationSending); updateErr != nil {
				return false, updateErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return false, commitErr
			}
			return false, ErrPublicationReconcile
		}
		if nowTime.Before(started.Add(outboundHandoffTimeout)) {
			return false, tx.Commit()
		}
		// The remote request may have succeeded after the local timeout. Do
		// not send it again; only marker reconciliation may complete it.
		if _, updateErr := tx.Exec(`UPDATE review_publications SET status = ?, service_lease_owner = '', service_lease_fence = 0
			WHERE id = ? AND review_id = ? AND status = ?`, PublicationReconciliationRequired, publicationID, reviewID, PublicationSending); updateErr != nil {
			return false, updateErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return false, commitErr
		}
		return false, ErrPublicationReconcile
	}
	res, err := tx.Exec(`UPDATE review_publications SET service_lease_owner = ?, service_lease_fence = ?, send_started_at = ''
			WHERE id = ? AND review_id = ? AND status = ?`,
		owner, fence, publicationID, reviewID, PublicationPending)
	if err != nil {
		return false, err
	}
	if err := requireReviewMutation(res); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// BeginPublicationSend is the fenced handoff immediately before a GitHub
// POST. Once it succeeds, any subsequent transport or acknowledgement failure
// is treated as remotely ambiguous and must reconcile the marker.
func (s *Store) BeginPublicationSend(reviewID, generation, publicationID int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, reviewID, generation, owner, fence); err != nil {
		return err
	}
	nowTime := time.Now().UTC()
	res, err := tx.Exec(`UPDATE review_publications SET status = ?, service_lease_owner = ?, service_lease_fence = ?, send_started_at = ?
			WHERE id = ? AND review_id = ? AND status = ? AND service_lease_owner = ? AND service_lease_fence = ?`,
		PublicationSending, owner, fence, serviceLeaseTime(nowTime), publicationID, reviewID, PublicationPending, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkPublicationPosted records a GitHub effect for the current execution.
// Reconciliation may call it repeatedly with the same publication safely.
func (s *Store) MarkPublicationPosted(reviewID, generation, publicationID, commentID int64, owner string, fence int64) error {
	if commentID < 1 {
		return errors.New("published comment ID must be positive")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, reviewID, generation, owner, fence); err != nil {
		return err
	}
	var kind, status, publicationOwner string
	var findingID int64
	var publicationFence int64
	err = tx.QueryRow(`SELECT kind, COALESCE(finding_id, 0), status, service_lease_owner, service_lease_fence
		FROM review_publications WHERE id = ? AND review_id = ?`, publicationID, reviewID).
		Scan(&kind, &findingID, &status, &publicationOwner, &publicationFence)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == PublicationPosted {
		return tx.Commit()
	}
	if status == PublicationReconciliationRequired {
		res, err := tx.Exec(`UPDATE review_publications SET status = ?, posted_comment_id = ?, posted_at = ?,
			service_lease_owner = '', service_lease_fence = 0, send_started_at = ''
			WHERE id = ? AND review_id = ? AND status = ?`, PublicationPosted, commentID, now(), publicationID, reviewID, PublicationReconciliationRequired)
		if err != nil {
			return err
		}
		if err := requireReviewMutation(res); err != nil {
			return err
		}
		return updatePublicationTarget(tx, kind, findingID, reviewID, generation, commentID)
	}
	if (status != PublicationSending && status != PublicationPending) || publicationOwner != owner || publicationFence != fence {
		return ErrReviewLeaseLost
	}
	res, err := tx.Exec(`UPDATE review_publications SET status = ?, posted_comment_id = ?, posted_at = ?,
			service_lease_owner = '', service_lease_fence = 0, send_started_at = ''
			WHERE id = ? AND review_id = ? AND status IN (?, ?) AND service_lease_owner = ? AND service_lease_fence = ?`,
		PublicationPosted, commentID, now(), publicationID, reviewID, PublicationSending, PublicationPending, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return updatePublicationTarget(tx, kind, findingID, reviewID, generation, commentID)
}

func updatePublicationTarget(tx *sql.Tx, kind string, findingID, reviewID, generation, commentID int64) error {
	if kind == PublicationSummary {
		if _, err := tx.Exec(`UPDATE reviews SET summary_comment_id = ?
				WHERE id = ? AND status = ? AND execution_generation = ?`, commentID, reviewID, StatusRunning, generation); err != nil {
			return err
		}
	} else if kind == PublicationInline {
		if _, err := tx.Exec(`UPDATE findings SET posted_comment_id = ? WHERE id = ? AND review_id = ?`,
			commentID, findingID, reviewID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkPublicationReconciliationRequired records that a remote POST may have
// succeeded but local acknowledgement was not durable. Callers may only search
// for the marker after this state is written; they must not create a replacement.
func (s *Store) MarkPublicationReconciliationRequired(reviewID, generation, publicationID int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, reviewID, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE review_publications SET status = ?, service_lease_owner = '', service_lease_fence = 0
		WHERE id = ? AND review_id = ? AND status IN (?, ?, ?)`, PublicationReconciliationRequired,
		publicationID, reviewID, PublicationSending, PublicationPending, PublicationReconciliationRequired)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishReviewReconciliationRequired makes an uncertain publication visible to
// operators without pretending that the remote effect was not accepted. It
// releases the execution lease; a later repair can only reconcile markers.
func (s *Store) FinishReviewReconciliationRequired(id, generation int64, errText string, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET status = ?, error = ?, finished_at = ?, next_attempt_at = ?, service_lease_owner = ''
				WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		StatusReconciliationRequired, errText, now(), time.Now().UTC().Add(reconciliationRetryDelay).Format(time.RFC3339), id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishReviewDone terminally completes a fully posted durable publication.
func (s *Store) FinishReviewDone(id, generation int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET status = ?, finished_at = ?
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ? AND publication_prepared = 1
			  AND EXISTS (SELECT 1 FROM review_publications WHERE review_id = reviews.id AND kind = ? AND status = ?)
			  AND NOT EXISTS (SELECT 1 FROM review_publications WHERE review_id = reviews.id AND status != ?)`,
		StatusDone, now(), id, StatusRunning, generation, owner, fence, PublicationSummary, PublicationPosted, PublicationPosted)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return tx.Commit()
	}
	return ErrReviewNotReady
}

// FinishReviewFailed stores a sanitized terminal error for only the current
// execution generation; a stale worker cannot overwrite a newer attempt.
func (s *Store) FinishReviewFailed(id, generation int64, errText string, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET status = ?, error = ?, finished_at = ?
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`, StatusFailed, errText, now(), id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

// RequeueReview releases a current review lease for a bounded retry without
// discarding a prepared publication plan.
func (s *Store) RequeueReview(id, generation int64, retryAfter time.Duration, owner string, fence int64) error {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	next := time.Now().UTC().Add(retryAfter).Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireReviewLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE reviews SET status = ?, started_at = '', next_attempt_at = ?, service_lease_owner = ''
					WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		StatusQueued, next, id, StatusRunning, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE review_publications SET service_lease_owner = '', service_lease_fence = 0
			WHERE review_id = ? AND status = ?`, id, PublicationPending); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListReviews(limit int) ([]Review, error) {
	return s.listReviews(`SELECT `+reviewCols+` FROM reviews ORDER BY id DESC LIMIT ?`, limit)
}

// ListReviewsForRequesterGitHubID scopes dashboard data by immutable identity.
func (s *Store) ListReviewsForRequesterGitHubID(githubID int64, limit int) ([]Review, error) {
	return s.listReviews(`SELECT `+reviewCols+` FROM reviews
		WHERE requester_github_id = ? ORDER BY id DESC LIMIT ?`, githubID, limit)
}

// ListReviewsForTargets filters immutable target IDs before applying limit so
// inaccessible historical records cannot crowd visible history off a page.
func (s *Store) ListReviewsForTargets(installationIDs, repositoryIDs []int64, limit int) ([]Review, error) {
	return s.listReviewsForTargets(0, installationIDs, repositoryIDs, limit)
}

// ListReviewsForRequesterGitHubIDAndTargets scopes history to both requester
// identity and currently authorized immutable review targets before limiting.
func (s *Store) ListReviewsForRequesterGitHubIDAndTargets(githubID int64, installationIDs, repositoryIDs []int64, limit int) ([]Review, error) {
	return s.listReviewsForTargets(githubID, installationIDs, repositoryIDs, limit)
}

func (s *Store) listReviewsForTargets(requesterGitHubID int64, installationIDs, repositoryIDs []int64, limit int) ([]Review, error) {
	if len(installationIDs) == 0 || len(repositoryIDs) == 0 {
		return []Review{}, nil
	}
	conditions := []string{
		"installation_id IN (" + reviewSQLPlaceholders(len(installationIDs)) + ")",
		"repository_id IN (" + reviewSQLPlaceholders(len(repositoryIDs)) + ")",
	}
	args := make([]any, 0, len(installationIDs)+len(repositoryIDs)+2)
	if requesterGitHubID > 0 {
		conditions = append([]string{"requester_github_id = ?"}, conditions...)
		args = append(args, requesterGitHubID)
	}
	for _, id := range installationIDs {
		args = append(args, id)
	}
	for _, id := range repositoryIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	return s.listReviews(`SELECT `+reviewCols+` FROM reviews WHERE `+strings.Join(conditions, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
}

func reviewSQLPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (s *Store) listReviews(query string, args ...any) ([]Review, error) {
	rows, err := s.db.Query(query, args...)
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

// SaveFinding is retained only as a compatibility guard for old callers. A
// finding without the matching fenced publication is not safe to persist.
func (s *Store) SaveFinding(f *Finding) error {
	return ErrUnfencedFinding
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

// CreateNudgeOnce persists a notification decision with the incoming webhook
// delivery. It prevents a replay from changing registration state into review
// access and produces one outbox record for concurrent mentions.
func (s *Store) CreateNudgeOnce(n *Nudge, deliveryID string) (NudgeCreateResult, error) {
	if err := validateNudge(n); err != nil {
		return NudgeCreateResult{}, err
	}
	token, err := newOpaqueToken()
	if err != nil {
		return NudgeCreateResult{}, fmt.Errorf("generate nudge token: %w", err)
	}
	n.RequesterLogin = strings.ToLower(strings.TrimSpace(n.RequesterLogin))
	deliveryID = strings.TrimSpace(deliveryID)
	tx, err := s.db.Begin()
	if err != nil {
		return NudgeCreateResult{}, err
	}
	defer tx.Rollback()
	if deliveryID != "" {
		created, err := claimDelivery(tx, deliveryID, now())
		if err != nil {
			return NudgeCreateResult{}, err
		}
		if !created {
			if err := tx.Commit(); err != nil {
				return NudgeCreateResult{}, err
			}
			return NudgeCreateResult{DuplicateDelivery: true}, nil
		}
	}
	if n.Kind == NudgeRegistration {
		var legacyNudge int
		err := tx.QueryRow(`SELECT 1 FROM register_nudges
			WHERE lower(repo_full) = lower(?) AND pr_number = ? AND lower(login) = lower(?)
			LIMIT 1`, n.RepoFull, n.PRNumber, n.RequesterLogin).Scan(&legacyNudge)
		if err == nil {
			if err := tx.Commit(); err != nil {
				return NudgeCreateResult{}, err
			}
			return NudgeCreateResult{}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return NudgeCreateResult{}, err
		}
	}
	res, err := tx.Exec(`INSERT INTO access_nudges
		(repo_full, repository_id, pr_number, installation_id, requester_github_id, requester_login, kind,
		 publication_token, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		n.RepoFull, n.RepositoryID, n.PRNumber, n.InstallationID, n.RequesterGitHubID, n.RequesterLogin,
		n.Kind, token, NudgeQueued, now())
	if err != nil {
		return NudgeCreateResult{}, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return NudgeCreateResult{}, err
	}
	if rows == 0 {
		if err := tx.Commit(); err != nil {
			return NudgeCreateResult{}, err
		}
		return NudgeCreateResult{}, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return NudgeCreateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return NudgeCreateResult{}, err
	}
	n.ID = id
	n.PublicationToken = token
	return NudgeCreateResult{Created: true}, nil
}

func validateNudge(n *Nudge) error {
	if n == nil || n.RepoFull == "" || n.RepositoryID < 1 || n.PRNumber < 1 || n.InstallationID < 1 ||
		n.RequesterGitHubID < 1 || n.RequesterLogin == "" || (n.Kind != NudgeRegistration && n.Kind != NudgeEntitlement) {
		return errors.New("nudge requires immutable identities and a known kind")
	}
	return nil
}

func scanNudge(row interface{ Scan(...any) error }) (*Nudge, error) {
	var n Nudge
	var createdAt, postedAt, nextAttemptAt, sendingStartedAt string
	err := row.Scan(&n.ID, &n.RepoFull, &n.RepositoryID, &n.PRNumber, &n.InstallationID, &n.RequesterGitHubID,
		&n.RequesterLogin, &n.Kind, &n.PublicationToken, &n.Status, &n.ExecutionGeneration, &n.ServiceLeaseOwner, &n.ClaimFence, &n.PostedCommentID,
		&nextAttemptAt, &createdAt, &postedAt, &sendingStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	n.PostedAt, _ = time.Parse(time.RFC3339, postedAt)
	n.NextAttemptAt, _ = time.Parse(time.RFC3339, nextAttemptAt)
	n.SendingStartedAt, _ = time.Parse(time.RFC3339, sendingStartedAt)
	return &n, nil
}

const nudgeCols = `id, repo_full, repository_id, pr_number, installation_id, requester_github_id,
	requester_login, kind, publication_token, status, execution_generation, service_lease_owner, claim_fence, posted_comment_id, next_attempt_at, created_at, posted_at, send_started_at`

func (s *Store) ClaimQueuedNudge(id int64) (*Nudge, bool, error) {
	return nil, false, ErrServiceLeaseRequired
}

func (s *Store) ClaimNudgeByID(id int64, owner string, fence int64) (*Nudge, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return nil, false, err
	}
	n, claimed, err := claimNudge(tx, id, owner, fence)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return n, claimed, nil
}

// ClaimNextQueuedNudge is fenced by the live singleton service lease.
func (s *Store) ClaimNextQueuedNudge(owner string, fence int64) (*Nudge, error) {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return nil, ErrServiceLeaseRequired
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return nil, err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM access_nudges
					WHERE status IN (?, ?) AND (next_attempt_at = '' OR next_attempt_at <= ?)
							AND (send_started_at = '' OR send_started_at <= ?)
							AND EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)
						ORDER BY id LIMIT 1`, NudgeQueued, NudgeReconciliationRequired, now(), leaseDeadline(time.Now().UTC()), owner, fence, serviceLeaseTime(time.Now().UTC())).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if leaseErr := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); leaseErr != nil {
				return nil, leaseErr
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	n, claimed, err := claimNudge(tx, id, owner, fence)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if leaseErr := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); leaseErr != nil {
			return nil, leaseErr
		}
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return n, nil
}

func claimNudge(tx *sql.Tx, id int64, owner string, fence int64) (*Nudge, bool, error) {
	var previousStatus string
	if err := tx.QueryRow(`SELECT status FROM access_nudges WHERE id = ?`, id).Scan(&previousStatus); err != nil {
		return nil, false, err
	}
	res, err := tx.Exec(`UPDATE access_nudges
						SET status = ?, execution_generation = execution_generation + 1, next_attempt_at = '',
						service_lease_owner = ?, claim_fence = ?
							WHERE id = ? AND status IN (?, ?) AND
							(next_attempt_at = '' OR next_attempt_at <= ?) AND
							(send_started_at = '' OR send_started_at <= ?) AND
							EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)`,
		NudgePosting, owner, fence, id, NudgeQueued, NudgeReconciliationRequired, now(), leaseDeadline(time.Now().UTC()), owner, fence, serviceLeaseTime(time.Now().UTC()))
	if err != nil {
		return nil, false, err
	}
	rows, err := res.RowsAffected()
	if err != nil || rows == 0 {
		return nil, rows > 0, err
	}
	n, err := scanNudge(tx.QueryRow(`SELECT `+nudgeCols+` FROM access_nudges WHERE id = ?`, id))
	if n != nil {
		n.ReconciliationRequired = previousStatus == NudgeReconciliationRequired
	}
	return n, true, err
}

// MarkNudgeSending records the point immediately before an ambiguous GitHub
// POST. Retries retain this timestamp until the handoff window expires.
func (s *Store) MarkNudgeSending(id, generation int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET send_started_at = ?
		WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		serviceLeaseTime(time.Now().UTC()), id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNudgeLeaseLost
	}
	return tx.Commit()
}

// RecoverInterruptedNudges distinguishes a claim that crashed before the send
// fence from a request that may already have reached GitHub. Only the latter
// becomes reconciliation-required; never-started claims are ordinary retries.
func (s *Store) RecoverInterruptedNudges(owner string, fence int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET status = ?, next_attempt_at = '', service_lease_owner = '', send_started_at = ''
				WHERE status = ? AND claim_fence < ? AND send_started_at = ''`,
		NudgeQueued, NudgePosting, fence)
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	res, err = tx.Exec(`UPDATE access_nudges SET status = ?, next_attempt_at = ?, service_lease_owner = ''
				WHERE status = ? AND claim_fence < ? AND send_started_at <> '' AND send_started_at <= ?`,
		NudgeReconciliationRequired, time.Now().UTC().Add(reconciliationRetryDelay).Format(time.RFC3339), NudgePosting, fence, leaseDeadline(time.Now().UTC()))
	if err != nil {
		return 0, err
	}
	reconciled, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count + reconciled), nil
}

func (s *Store) NudgeLeaseCurrent(id, generation int64, owner string, fence int64) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM access_nudges
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?
			  AND EXISTS (SELECT 1 FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?)`,
		id, NudgePosting, generation, owner, fence, owner, fence, serviceLeaseTime(time.Now().UTC())).Scan(&count)
	return count == 1, err
}

// MarkNudgePosted commits a reconciled or newly created notification ID.
func (s *Store) MarkNudgePosted(id, generation, commentID int64, owner string, fence int64) error {
	if commentID < 1 {
		return errors.New("published comment ID must be positive")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET status = ?, posted_comment_id = ?, posted_at = ?, service_lease_owner = '', send_started_at = ''
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		NudgePosted, commentID, now(), id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNudgeLeaseLost
	}
	return tx.Commit()
}

// RequeueNudge leaves a failed outbound attempt durable for a bounded retry.
func (s *Store) RequeueNudge(id, generation int64, retryAfter time.Duration, owner string, fence int64) error {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	next := time.Now().UTC().Add(retryAfter).Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	var sendStartedAt string
	if err := tx.QueryRow(`SELECT send_started_at FROM access_nudges WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		id, NudgePosting, generation, owner, fence).Scan(&sendStartedAt); err != nil {
		return err
	}
	if sendStartedAt != "" {
		if _, err := tx.Exec(`UPDATE access_nudges SET status = ?, next_attempt_at = ?, service_lease_owner = ''
				WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
			NudgeReconciliationRequired, time.Now().UTC().Add(reconciliationRetryDelay).Format(time.RFC3339), id, NudgePosting, generation, owner, fence); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrNudgeReconcile
	}
	res, err := tx.Exec(`UPDATE access_nudges SET status = ?, next_attempt_at = ?, service_lease_owner = ''
				WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		NudgeQueued, next, id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNudgeLeaseLost
	}
	return tx.Commit()
}

// FinishNudgeFailed ends a notification that cannot be safely delivered. It
// is generation-fenced so an old worker cannot discard a newer retry.
func (s *Store) FinishNudgeFailed(id, generation int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET status = ?, service_lease_owner = '', send_started_at = ''
			WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`, NudgeFailed, id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNudgeLeaseLost
	}
	return tx.Commit()
}

// MarkNudgeReconciliationRequired preserves unknown delivery after a GitHub
// request began. The marker lookup on the next claim is the only safe recovery
// path; this transition is idempotent for the current fenced worker.
func (s *Store) MarkNudgeReconciliationRequired(id, generation int64, owner string, fence int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := requireNudgeLeaseTx(tx, id, generation, owner, fence); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE access_nudges SET status = ?, next_attempt_at = ?, service_lease_owner = ''
				WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		NudgeReconciliationRequired, time.Now().UTC().Add(reconciliationRetryDelay).Format(time.RFC3339), id, NudgePosting, generation, owner, fence)
	if err != nil {
		return err
	}
	if err := requireReviewMutation(res); err != nil {
		return err
	}
	return tx.Commit()
}

func requireNudgeLeaseTx(tx *sql.Tx, id, generation int64, owner string, fence int64) error {
	if err := requireServiceLeaseTx(tx, owner, fence, time.Now().UTC()); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM access_nudges
		WHERE id = ? AND status = ? AND execution_generation = ? AND service_lease_owner = ? AND claim_fence = ?`,
		id, NudgePosting, generation, owner, fence).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrNudgeLeaseLost
	}
	return nil
}

// NudgeMarker is an opaque marker used to reconcile an ambiguous GitHub POST.
func NudgeMarker(token string) string {
	return fmt.Sprintf("<!-- oc-review-bot:v1:nudge:%s -->", token)
}
