// Package bot runs queued reviews end to end: GitHub fetch, agent run,
// finding mapping, and durable comment publication.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jR4dh3y/samik-bot/internal/config"
	"github.com/jR4dh3y/samik-bot/internal/gh"
	"github.com/jR4dh3y/samik-bot/internal/pool"
	"github.com/jR4dh3y/samik-bot/internal/review"
	"github.com/jR4dh3y/samik-bot/internal/runner"
	"github.com/jR4dh3y/samik-bot/internal/store"
	"modernc.org/sqlite"
)

const (
	reviewFailureMessage       = "The review could not be completed safely. Please mention the bot again to retry."
	headChangedMessage         = "The pull request changed before the review could complete. Mention the bot again to review the latest revision."
	reviewAccessRevokedMessage = "Review access was removed before this queued request could run."
	invalidModelMessage        = "The configured review model is no longer valid. An administrator must update it in the dashboard before retrying."
	failureReaction            = "-1"

	queuePollInterval   = time.Second
	transientRetryDelay = 15 * time.Second
	maxDeliveryAttempts = int64(5)
	nudgeRequestTimeout = 30 * time.Second
	serviceLeaseTTL     = 30 * time.Second
	deliveryRetention   = 7 * 24 * time.Hour
	deliveryPurgeEvery  = 6 * time.Hour
)

var (
	// ErrAlreadyStarted prevents two worker pools from racing over one Engine.
	ErrAlreadyStarted = errors.New("review engine already started")
	// ErrServiceLeaseUnavailable means another service process owns the shared
	// database's worker lease. Starting without it would race durable recovery.
	ErrServiceLeaseUnavailable = errors.New("review engine service lease unavailable")
	errReviewAccess            = errors.New("review requester is no longer authorized")
	errPublicationInFlight     = errors.New("review publication is in flight")
	errRepositoryIdentity      = errors.New("pull request repository identity changed")
	errReviewRevision          = errors.New("pull request revision token is missing or changed")
)

const reconciliationReviewError = "A GitHub delivery may have succeeded, but its local acknowledgement was not durable. Operator reconciliation is required before retrying."

type reviewRunner func(context.Context, runner.Options) (string, error)

// Engine consumes the SQLite-backed review and notification outboxes. wake is
// only an optimization: durable claims, not in-memory IDs, decide what runs.
type Engine struct {
	cfg  *config.Config
	st   *store.Store
	app  *gh.App
	pool *pool.Pool
	log  *slog.Logger
	run  reviewRunner

	wake         chan struct{}
	startMu      sync.Mutex
	started      bool
	stopping     bool
	ready        bool
	cancel       context.CancelFunc
	leaseOwner   string
	leaseFence   int64
	recoveryMu   sync.Mutex
	stopDone     chan struct{}
	stopErr      error
	wg           sync.WaitGroup
	pollInterval time.Duration
	leaseTTL     time.Duration

	lastDeliveryPurge time.Time
}

func NewEngine(cfg *config.Config, st *store.Store, app *gh.App, p *pool.Pool, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		cfg:          cfg,
		st:           st,
		app:          app,
		pool:         p,
		log:          log,
		run:          runner.Run,
		wake:         make(chan struct{}, 1),
		pollInterval: queuePollInterval,
		leaseTTL:     serviceLeaseTTL,
	}
}

// Start synchronously recovers work from an interrupted process, then launches
// n durable-queue workers. It must succeed before the HTTP server starts. The
// database lease is acquired before recovery so two service processes cannot
// both turn the same running rows back into queue entries.
func (e *Engine) Start(n int) error {
	if n < 1 {
		n = 1
	}
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if e.started || e.stopping {
		return ErrAlreadyStarted
	}
	if e.st == nil {
		return errors.New("review engine store is required")
	}
	owner, err := newServiceLeaseOwner()
	if err != nil {
		return fmt.Errorf("create service lease owner: %w", err)
	}
	leaseTTL := e.leaseTTL
	if leaseTTL <= 0 {
		leaseTTL = serviceLeaseTTL
	}
	lease, acquired, err := e.st.AcquireServiceLeaseWithFence(owner, leaseTTL)
	if err != nil {
		return fmt.Errorf("acquire service lease: %w", err)
	}
	if !acquired {
		return ErrServiceLeaseUnavailable
	}
	releaseOnError := func(startErr error) error {
		if _, releaseErr := e.st.ReleaseServiceLease(owner, lease.Fence); releaseErr != nil {
			return fmt.Errorf("%w; release service lease: %v", startErr, releaseErr)
		}
		return startErr
	}
	if _, err := e.st.RecoverInterruptedReviews(owner, lease.Fence); err != nil {
		return releaseOnError(fmt.Errorf("recover interrupted reviews: %w", err))
	}
	if _, err := e.st.RecoverInterruptedNudges(owner, lease.Fence); err != nil {
		return releaseOnError(fmt.Errorf("recover interrupted access notifications: %w", err))
	}
	// Startup purge runs before the lease is handed to workers; retention is
	// best-effort and must not fail startup.
	e.purgeDeliveryMarkers()
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.leaseOwner = owner
	e.leaseFence = lease.Fence
	e.started = true
	e.ready = true
	e.stopErr = nil
	e.wg.Add(n + 1)
	go e.renewServiceLease(ctx, leaseTTL)
	for i := 0; i < n; i++ {
		go e.worker(ctx)
	}
	return nil
}

// Ready reports whether startup completed and this process still owns the
// durable worker lease. A lease loss makes the process unready immediately;
// the renewal goroutine also cancels workers so they cannot claim more work.
func (e *Engine) Ready() bool {
	e.startMu.Lock()
	started, ready, owner, fence := e.started, e.ready, e.leaseOwner, e.leaseFence
	e.startMu.Unlock()
	if !started || !ready || owner == "" || fence < 1 || e.st == nil {
		return false
	}
	current, err := e.st.ServiceLeaseCurrent(owner, fence)
	if err != nil || !current {
		e.loseServiceLease(owner)
		return false
	}
	return true
}

// Stop cancels all workers, waits for in-flight work to leave its bounded
// context, and releases the singleton lease. It is safe to call more than
// once; a timed-out caller leaves a cleanup waiter running so the lease is
// still released after the workers finish.
func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.startMu.Lock()
	if e.stopping {
		done := e.stopDone
		e.startMu.Unlock()
		select {
		case <-done:
			e.startMu.Lock()
			err := e.stopErr
			e.startMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !e.started {
		e.startMu.Unlock()
		return nil
	}
	e.stopping = true
	e.started = false
	e.ready = false
	owner := e.leaseOwner
	fence := e.leaseFence
	cancel := e.cancel
	done := make(chan struct{})
	e.stopDone = done
	e.startMu.Unlock()

	if cancel != nil {
		cancel()
	}
	waitDone := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(waitDone)
	}()
	finish := func() {
		var stopErr error
		if owner != "" && fence > 0 && e.st != nil {
			if _, err := e.st.ReleaseServiceLease(owner, fence); err != nil {
				stopErr = fmt.Errorf("release service lease: %w", err)
			}
		}
		e.startMu.Lock()
		e.stopErr = stopErr
		e.stopping = false
		e.ready = false
		e.leaseOwner = ""
		e.leaseFence = 0
		e.cancel = nil
		e.stopDone = nil
		e.startMu.Unlock()
		close(done)
	}
	select {
	case <-waitDone:
		finish()
	case <-ctx.Done():
		go func() {
			<-waitDone
			finish()
		}()
		return ctx.Err()
	}
	e.startMu.Lock()
	err := e.stopErr
	e.startMu.Unlock()
	return err
}

func newServiceLeaseOwner() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (e *Engine) renewServiceLease(ctx context.Context, ttl time.Duration) {
	defer e.wg.Done()
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			owner, fence := e.currentLease()
			if owner == "" {
				return
			}
			ok, err := e.st.RenewServiceLease(owner, fence, ttl)
			if err != nil {
				e.log.Error("renew service lease", "err", err)
				e.loseServiceLease(owner)
				return
			}
			if !ok {
				e.log.Error("service lease lost")
				e.loseServiceLease(owner)
				return
			}
		}
	}
}

func (e *Engine) currentLease() (string, int64) {
	e.startMu.Lock()
	defer e.startMu.Unlock()
	if !e.started || !e.ready {
		return "", 0
	}
	return e.leaseOwner, e.leaseFence
}

func (e *Engine) currentLeaseOwner() string {
	owner, _ := e.currentLease()
	return owner
}

func (e *Engine) currentLeaseFence() int64 {
	_, fence := e.currentLease()
	return fence
}

func (e *Engine) loseServiceLease(owner string) {
	e.startMu.Lock()
	if e.leaseOwner != owner || (!e.started && !e.stopping) {
		e.startMu.Unlock()
		return
	}
	e.ready = false
	cancel := e.cancel
	e.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *Engine) serviceLeaseOwned() bool {
	e.startMu.Lock()
	started, stopping, owner, fence := e.started, e.stopping, e.leaseOwner, e.leaseFence
	e.startMu.Unlock()
	if !started && !stopping {
		return true
	}
	if !started || owner == "" || fence < 1 || e.st == nil {
		return false
	}
	ok, err := e.st.ServiceLeaseCurrent(owner, fence)
	if err != nil || !ok {
		e.loseServiceLease(owner)
		return false
	}
	return true
}

func (e *Engine) mayProcess(ctx context.Context) bool {
	if !e.serviceLeaseOwned() {
		return false
	}
	return ctx == nil || ctx.Err() == nil
}

func (e *Engine) processError(ctx context.Context) error {
	if !e.serviceLeaseOwned() {
		return store.ErrReviewLeaseLost
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// Enqueue wakes a worker after a review was committed. The row is already
// durable, so a full wake channel must never make webhook delivery block.
func (e *Engine) Enqueue(_ int64) {
	e.signal()
}

// EnqueueNudge wakes a worker after an access notification was committed.
func (e *Engine) EnqueueNudge(_ int64) {
	e.signal()
}

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) worker(ctx context.Context) {
	defer e.wg.Done()
	preferNudge := false
	for {
		if ctx.Err() != nil || !e.serviceLeaseOwned() {
			return
		}
		if !e.recoverExpiredWork() {
			return
		}
		if e.claimAndProcess(ctx, preferNudge) {
			preferNudge = !preferNudge
			continue
		}
		preferNudge = !preferNudge

		interval := e.pollInterval
		if interval <= 0 {
			interval = queuePollInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-e.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// recoverExpiredWork runs after the startup recovery pass as well. A worker
// successor may start while an old HTTP handoff is still protected, so stale
// outbox rows must become claimable without requiring another process restart.
func (e *Engine) recoverExpiredWork() bool {
	e.recoveryMu.Lock()
	defer e.recoveryMu.Unlock()
	owner, fence := e.currentLease()
	if owner == "" || fence < 1 {
		return false
	}
	if _, err := e.st.RecoverInterruptedReviews(owner, fence); err != nil {
		if errors.Is(err, store.ErrServiceLeaseLost) {
			e.loseServiceLease(owner)
			return false
		}
		e.log.Error("recover interrupted reviews", "err", err)
	}
	if _, err := e.st.RecoverInterruptedNudges(owner, fence); err != nil {
		if errors.Is(err, store.ErrServiceLeaseLost) {
			e.loseServiceLease(owner)
			return false
		}
		e.log.Error("recover interrupted access notifications", "err", err)
	}
	e.purgeDeliveryMarkers()
	return e.serviceLeaseOwned()
}

// purgeDeliveryMarkers bounds the webhook replay table on a fixed cadence.
// Retention is deliberately longer than GitHub's redelivery window so a
// late redelivery is still recognized as a duplicate.
func (e *Engine) purgeDeliveryMarkers() {
	if time.Since(e.lastDeliveryPurge) < deliveryPurgeEvery {
		return
	}
	if _, err := e.st.PurgeDeliveriesBefore(time.Now().Add(-deliveryRetention)); err != nil {
		e.log.Error("purge webhook delivery markers", "err", err)
		return
	}
	e.lastDeliveryPurge = time.Now()
}

func (e *Engine) claimAndProcess(ctx context.Context, preferNudge bool) bool {
	if preferNudge {
		if e.claimAndProcessNudge(ctx) {
			return true
		}
		return e.claimAndProcessReview(ctx)
	}
	if e.claimAndProcessReview(ctx) {
		return true
	}
	return e.claimAndProcessNudge(ctx)
}

func (e *Engine) claimAndProcessReview(ctx context.Context) bool {
	if !e.mayProcess(ctx) {
		return false
	}
	owner, fence := e.currentLease()
	if owner == "" || fence < 1 {
		return false
	}
	r, err := e.st.ClaimNextQueuedReview(owner, fence)
	if errors.Is(err, store.ErrServiceLeaseLost) {
		e.loseServiceLease(owner)
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		e.log.Error("claim queued review", "err", err)
		return false
	}
	e.recordEvent(r.ID, "prepare", "Claimed for processing")
	ctx, cancel := context.WithTimeout(ctx, e.reviewTimeout())
	e.processClaimedReview(ctx, r)
	cancel()
	return true
}

func (e *Engine) claimAndProcessNudge(ctx context.Context) bool {
	if !e.mayProcess(ctx) {
		return false
	}
	owner, fence := e.currentLease()
	if owner == "" || fence < 1 {
		return false
	}
	n, err := e.st.ClaimNextQueuedNudge(owner, fence)
	if errors.Is(err, store.ErrServiceLeaseLost) {
		e.loseServiceLease(owner)
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		e.log.Error("claim queued access notification", "err", err)
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, nudgeRequestTimeout)
	e.processClaimedNudge(ctx, n)
	cancel()
	return true
}

func (e *Engine) reviewTimeout() time.Duration {
	if e.cfg != nil && e.cfg.ReviewTimeout > 0 {
		return e.cfg.ReviewTimeout
	}
	return 20 * time.Minute
}

// recordEvent best-effort persists a dashboard progress event. A failure only
// loses observability and must never affect the review pipeline.
func (e *Engine) recordEvent(reviewID int64, kind, message string) {
	if err := e.st.AppendReviewEvent(reviewID, kind, message); err != nil {
		e.log.Warn("record review event", "review", reviewID, "kind", kind, "err", err)
	}
}

// runnerProgress maps runner lifecycle stages to dashboard events. Unknown
// stages fall back to the raw stage name and must never be emitted.
func (e *Engine) runnerProgress(reviewID int64) func(stage string) {
	return func(stage string) {
		kind, message := "prepare", stage
		switch stage {
		case runner.StageCheckoutStart:
			kind, message = "checkout", "Fetching the repository snapshot"
		case runner.StageCheckoutRetry:
			kind, message = "checkout", "Retrying the repository snapshot"
		case runner.StageCheckoutDone:
			kind, message = "checkout", "Repository snapshot ready"
		case runner.StageSandboxReady:
			kind, message = "sandbox", "Sandbox boundary established"
		case runner.StageSandboxSealed:
			kind, message = "sandbox", "Sandbox sealed with isolated credentials"
		case runner.StageAgentStart:
			kind, message = "agent_started", "Reviewer agent started"
		case runner.StageAgentDone:
			kind, message = "agent_finished", "Reviewer agent finished"
		}
		e.recordEvent(reviewID, kind, message)
	}
}

// processOne is retained as a focused test helper. Production workers claim
// directly from the durable queue instead of receiving an in-memory ID.
func (e *Engine) processOne(ctx context.Context, id int64) {
	owner, fence := e.currentLease()
	if owner == "" || fence < 1 {
		e.log.Error("claim review", "id", id, "err", store.ErrServiceLeaseRequired)
		return
	}
	r, claimed, err := e.st.ClaimReviewByID(id, owner, fence)
	if err != nil {
		e.log.Error("claim review", "id", id, "err", err)
		return
	}
	if !claimed {
		e.log.Debug("review already claimed or finished", "id", id)
		return
	}
	e.processClaimedReview(ctx, r)
}

func (e *Engine) processClaimedReview(ctx context.Context, r *store.Review) {
	if r == nil {
		return
	}
	if !e.mayProcess(ctx) {
		return
	}
	log := e.log.With("review", r.ID, "repo", r.RepoFull, "pr", r.PRNumber)
	if !e.cfg.CanRequestReview(r.RequesterGitHubID, r.InstallationID, r.RepositoryID) {
		if e.failReview(r, log, "access_revoked", reviewAccessRevokedMessage) {
			e.signalReviewFailure(ctx, "", r, log)
		}
		return
	}

	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	token, err := e.app.InstallationToken(ctx, r.InstallationID, r.RepositoryID)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.recordEvent(r.ID, "prepare", "Fetched the GitHub installation token")
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	currentPR, err := e.app.GetPR(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	verifiedRepo, err := verifyPRTarget(r.RepositoryID, r.PRNumber, currentPR)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	r.RepoFull = verifiedRepo
	currentRevision := currentPR.RevisionToken()
	if currentRevision == "" {
		e.handleReviewError(ctx, token, r, log, errReviewRevision)
		return
	}
	if r.PublicationPrepared {
		if r.HeadRevision == "" || r.HeadRevision != currentRevision || !strings.EqualFold(currentPR.Head.SHA, r.HeadSHA) {
			e.handlePreparedHeadChanged(ctx, token, r, log)
			return
		}
	} else if (r.HeadSHA != "" && !strings.EqualFold(currentPR.Head.SHA, r.HeadSHA)) ||
		(r.HeadRevision != "" && r.HeadRevision != currentRevision) {
		e.failHeadChanged(r, log)
		return
	}
	if err := e.st.SetReviewRevision(r.ID, r.ExecutionGeneration, verifiedRepo, currentPR.Head.SHA, currentRevision, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	r.HeadSHA = currentPR.Head.SHA
	r.HeadRevision = currentRevision
	e.recordEvent(r.ID, "prepare", fmt.Sprintf("Verified the pull-request head (sha %.7s)", currentPR.Head.SHA))
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	if _, err := e.ensureCurrentRevision(ctx, token, r); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		if !errors.Is(err, store.ErrReviewLeaseLost) {
			e.handleReviewError(ctx, token, r, log, err)
		}
		return
	}
	if err := e.app.ReactToIssueComment(ctx, token, r.RepoFull, r.TriggerCommentID, "eyes"); err != nil {
		log.Warn("acknowledge review request", "cause", "github")
	}
	if r.PublicationPrepared {
		// Re-read immediately before resuming a prepared publication. The
		// earlier revision check protects the common path; this one catches a
		// force-push while the worker was acknowledging the request.
		if _, err := e.ensureCurrentRevision(ctx, token, r); err != nil {
			if errors.Is(err, runner.ErrHeadChanged) {
				e.handlePreparedHeadChanged(ctx, token, r, log)
			} else {
				e.handleReviewError(ctx, token, r, log, err)
			}
			return
		}
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return
		}
		if err := e.publishPrepared(ctx, token, r); err != nil {
			e.handleReviewError(ctx, token, r, log, err)
			return
		}
		e.finishPublishedReview(ctx, token, r, log)
		return
	}

	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	if _, err := e.ensureCurrentRevision(ctx, token, r); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	files, err := e.app.ListFiles(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.recordEvent(r.ID, "prepare", fmt.Sprintf("Loaded the changed-file list (%d files)", len(files)))
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	if _, err := e.ensureCurrentRevision(ctx, token, r); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	diff, err := e.app.GetDiff(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.recordEvent(r.ID, "prepare", "Fetched the pull-request diff")

	model := e.st.GetSetting("model", e.cfg.DefaultModel)
	if !config.ValidModel(model) {
		if e.failReview(r, log, "invalid_model", invalidModelMessage) {
			e.signalReviewFailure(ctx, token, r, log)
		}
		return
	}
	_, agentOut, err := e.runWithPool(ctx, token, r, model, diff, log)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}

	// A force-push after the agent starts makes its output stale. Recheck the
	// exact revision before persisting a publication plan or writing to GitHub.
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	currentPR, err = e.app.GetPR(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	verifiedRepo, err = verifyPRTarget(r.RepositoryID, r.PRNumber, currentPR)
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	r.RepoFull = verifiedRepo
	if currentPR.RevisionToken() != r.HeadRevision || !strings.EqualFold(currentPR.Head.SHA, r.HeadSHA) {
		e.failHeadChanged(r, log)
		return
	}

	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	keptFindings, err := e.preparePublication(r, currentPR, review.NewDiffIndex(files), review.ExtractReview(agentOut))
	if err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.recordEvent(r.ID, "publish", fmt.Sprintf("Prepared the summary and %d inline findings for posting", keptFindings))
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return
	}
	if err := e.publishPrepared(ctx, token, r); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.finishPublishedReview(ctx, token, r, log)
}

// runWithPool runs the agent, retrying once on a quota failure with a second
// key. It rechecks entitlement and the execution fence before consuming a key.
func (e *Engine) runWithPool(ctx context.Context, token string, r *store.Review, model string, diff []byte, log *slog.Logger) (*store.ZenKey, string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return nil, "", err
		}
		if !e.cfg.CanRequestReview(r.RequesterGitHubID, r.InstallationID, r.RepositoryID) {
			return nil, "", errReviewAccess
		}
		current, err := e.st.ReviewLeaseCurrent(r.ID, r.ExecutionGeneration, r.ServiceLeaseOwner, r.ClaimFence)
		if err != nil {
			return nil, "", err
		}
		if !current {
			return nil, "", store.ErrReviewLeaseLost
		}
		key, err := e.pool.Acquire()
		if err != nil {
			return nil, "", fmt.Errorf("acquire key: %w", err)
		}
		if err := e.st.SetReviewExecution(r.ID, r.ExecutionGeneration, model, key.ID, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
			return nil, "", fmt.Errorf("record review execution: %w", err)
		}
		e.recordEvent(r.ID, "prepare", fmt.Sprintf("Acquired Zen credential %q", key.Label))
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return nil, "", err
		}
		engine, bin, runtimeDir := e.cfg.AgentRuntime()
		// The opencode isolation flags belong to the opencode2 engine alone; a
		// pi run must never inherit them even if both configs are populated.
		runArgs := e.cfg.OpenCodeArgs
		if engine == config.EnginePi {
			runArgs = nil
		}
		r.Model = model
		out, runErr := e.run(ctx, runner.Options{
			Engine:        engine,
			Bin:           bin,
			RuntimeDir:    runtimeDir,
			BubblewrapBin: e.cfg.BubblewrapBin,
			RunArgs:       runArgs,
			CloneURL:      fmt.Sprintf("https://github.com/%s.git", r.RepoFull),
			GitHubToken:   token,
			Ref:           fmt.Sprintf("refs/pull/%d/head", r.PRNumber),
			ExpectedSHA:   r.HeadSHA,
			Model:         model,
			APIKey:        key.Secret,
			Diff:          diff,
			Prompt:        review.BuildPrompt("review-diff.patch"),
			OnProgress:    e.runnerProgress(r.ID),
		})
		if runErr == nil {
			return key, out, nil
		}
		if !errors.Is(runErr, runner.ErrQuota) {
			return key, "", runErr
		}
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return nil, "", err
		}
		if err := e.pool.Exhausted(key.ID); err != nil {
			return key, "", fmt.Errorf("cool exhausted key: %w", err)
		}
		log.Warn("zen key exhausted, cooling down", "key_id", key.ID)
		lastErr = runErr
	}
	return nil, "", lastErr
}

// preparePublication persists the publication plan and returns the number of
// inline findings kept in it.
func (e *Engine) preparePublication(r *store.Review, pr *gh.PR, idx *review.DiffIndex, result review.ReviewResult) (int, error) {
	filtered := review.ReviewResult{
		SummaryMD:       result.SummaryMD,
		SequenceDiagram: result.SequenceDiagram,
	}
	plan := store.PublicationPlan{
		SummaryMD: result.SummaryMD,
		CommitSHA: pr.Head.SHA,
	}
	for _, finding := range result.Findings {
		if !idx.InDiff(finding.Path, finding.Side, finding.Line) {
			continue
		}
		filtered.Findings = append(filtered.Findings, finding)
		plan.Findings = append(plan.Findings, store.PublicationFinding{
			Path:          finding.Path,
			Line:          finding.Line,
			Side:          finding.Side,
			Severity:      finding.Severity,
			FindingBodyMD: finding.Body,
			CommentBodyMD: review.RenderInlineBody(finding),
		})
	}
	plan.SummaryBodyMD = review.RenderSummaryComment(filtered, e.cfg.BotUsername, r.Model)
	return len(plan.Findings), e.st.PrepareReviewPublication(r.ID, r.ExecutionGeneration, plan, r.ServiceLeaseOwner, r.ClaimFence)
}

// publishPrepared reconciles every opaque marker before issuing a write. A
// crash after GitHub accepts a POST therefore resumes without a duplicate.
func (e *Engine) publishPrepared(ctx context.Context, token string, r *store.Review) error {
	publications, err := e.st.ListReviewPublications(r.ID)
	if err != nil {
		return err
	}
	for _, publication := range publications {
		if publication.Status == store.PublicationPosted {
			continue
		}
		if publication.Status == store.PublicationReconciliationRequired {
			if err := e.reconcilePublication(ctx, token, r, publication); err != nil {
				return err
			}
			continue
		}
		if err := e.processError(ctx); err != nil {
			return err
		}
		claimed, err := e.st.ClaimPublicationForSend(r.ID, r.ExecutionGeneration, publication.ID, r.ServiceLeaseOwner, r.ClaimFence)
		if err != nil {
			if errors.Is(err, store.ErrPublicationReconcile) {
				// ClaimPublicationForSend has already moved an expired send into
				// reconciliation_required. Never turn that uncertainty into a
				// replacement POST.
				if reconcileErr := e.reconcilePublication(ctx, token, r, publication); reconcileErr != nil {
					return reconcileErr
				}
				continue
			}
			return err
		}
		if !claimed {
			current, listErr := e.st.ListReviewPublications(r.ID)
			if listErr != nil {
				return listErr
			}
			posted := false
			for _, candidate := range current {
				if candidate.ID == publication.ID {
					posted = candidate.Status == store.PublicationPosted
					break
				}
			}
			if posted {
				continue
			}
			return errPublicationInFlight
		}
		if _, err := e.ensureCurrentHead(ctx, token, r, publication.CommitSHA); err != nil {
			return err
		}

		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return err
		}
		commentID, found, err := e.findPublication(ctx, token, r, publication)
		if err != nil {
			return err
		}
		if !found {
			if err := e.reviewSideEffectError(ctx, r); err != nil {
				return err
			}
			if _, err := e.ensureCurrentHead(ctx, token, r, publication.CommitSHA); err != nil {
				return err
			}
			// Repeat the marker read after the final revision check so a
			// concurrent reconciler cannot make an already-posted effect look
			// absent immediately before this worker's POST.
			if existingID, existing, err := e.findPublication(ctx, token, r, publication); err != nil {
				return err
			} else if existing {
				commentID = existingID
				found = true
			}
		}
		if !found {
			if err := e.reviewSideEffectError(ctx, r); err != nil {
				return err
			}
			if err := e.st.BeginPublicationSend(r.ID, r.ExecutionGeneration, publication.ID, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
				return err
			}
			if err := e.reviewSideEffectError(ctx, r); err != nil {
				return e.publicationUncertain(r, publication, err)
			}
			commentID, err = e.createPublication(ctx, token, r, publication)
			if err != nil {
				return e.publicationUncertain(r, publication, err)
			}
		}
		if err := e.processError(ctx); err != nil {
			return e.publicationUncertain(r, publication, err)
		}
		if err := e.st.MarkPublicationPosted(r.ID, r.ExecutionGeneration, publication.ID, commentID, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
			return e.publicationUncertain(r, publication, err)
		}
	}
	if err := e.processError(ctx); err != nil {
		return err
	}
	return e.st.FinishReviewDone(r.ID, r.ExecutionGeneration, r.ServiceLeaseOwner, r.ClaimFence)
}

// reconcilePublication is marker-only. An absent marker remains an operator
// decision point; creating a replacement comment would duplicate an unknown
// remote effect. The PR head is deliberately not rechecked here: recording an
// effect that already landed is safe on any revision, and refusing the lookup
// on a force-push would orphan the uncertainty instead of resolving it.
func (e *Engine) reconcilePublication(ctx context.Context, token string, r *store.Review, publication store.Publication) error {
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return errors.Join(store.ErrPublicationReconcile, err)
	}
	commentID, found, err := e.findPublication(ctx, token, r, publication)
	if err != nil {
		return errors.Join(store.ErrPublicationReconcile, err)
	}
	if !found {
		return store.ErrPublicationReconcile
	}
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return errors.Join(store.ErrPublicationReconcile, err)
	}
	if err := e.st.MarkPublicationPosted(r.ID, r.ExecutionGeneration, publication.ID, commentID, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
		return errors.Join(store.ErrPublicationReconcile, err)
	}
	return nil
}

func (e *Engine) publicationUncertain(r *store.Review, publication store.Publication, cause error) error {
	if markErr := e.st.MarkPublicationReconciliationRequired(r.ID, r.ExecutionGeneration, publication.ID, r.ServiceLeaseOwner, r.ClaimFence); markErr != nil {
		return errors.Join(store.ErrPublicationReconcile, cause, markErr)
	}
	return errors.Join(store.ErrPublicationReconcile, cause)
}

func (e *Engine) ensureCurrentRevision(ctx context.Context, token string, r *store.Review) (*gh.PR, error) {
	if err := e.reviewSideEffectError(ctx, r); err != nil {
		return nil, err
	}
	pr, err := e.app.GetPR(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		return nil, err
	}
	verifiedRepo, err := verifyPRTarget(r.RepositoryID, r.PRNumber, pr)
	if err != nil {
		return nil, err
	}
	r.RepoFull = verifiedRepo
	revision := pr.RevisionToken()
	if revision == "" || r.HeadRevision == "" {
		return nil, errReviewRevision
	}
	if revision != r.HeadRevision || !strings.EqualFold(pr.Head.SHA, r.HeadSHA) {
		return nil, errors.Join(errReviewRevision, runner.ErrHeadChanged)
	}
	if err := e.st.SetReviewRevision(r.ID, r.ExecutionGeneration, verifiedRepo, r.HeadSHA, r.HeadRevision, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
		return nil, err
	}
	return pr, nil
}

func (e *Engine) ensureCurrentHead(ctx context.Context, token string, r *store.Review, expectedSHA string) (*gh.PR, error) {
	pr, err := e.ensureCurrentRevision(ctx, token, r)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(pr.Head.SHA, expectedSHA) {
		return nil, runner.ErrHeadChanged
	}
	return pr, nil
}

func (e *Engine) findPublication(ctx context.Context, token string, r *store.Review, publication store.Publication) (int64, bool, error) {
	switch publication.Kind {
	case store.PublicationSummary:
		return e.app.FindIssueCommentMarker(ctx, token, r.RepoFull, r.PRNumber, publication.Marker)
	case store.PublicationInline:
		return e.app.FindReviewCommentMarker(ctx, token, r.RepoFull, r.PRNumber, publication.Marker)
	default:
		return 0, false, errors.New("unknown review publication kind")
	}
}

func (e *Engine) createPublication(ctx context.Context, token string, r *store.Review, publication store.Publication) (int64, error) {
	switch publication.Kind {
	case store.PublicationSummary:
		return e.app.CreateIssueComment(ctx, token, r.RepoFull, r.PRNumber, publication.BodyMD)
	case store.PublicationInline:
		return e.app.CreateReviewComment(ctx, token, r.RepoFull, r.PRNumber, gh.ReviewComment{
			CommitID: publication.CommitSHA,
			Path:     publication.Path,
			Side:     publication.Side,
			Line:     publication.Line,
			Body:     publication.BodyMD,
		})
	default:
		return 0, errors.New("unknown review publication kind")
	}
}

func (e *Engine) finishPublishedReview(ctx context.Context, token string, r *store.Review, log *slog.Logger) {
	if err := e.reviewCompletionSideEffectError(ctx, r); err != nil {
		return
	}
	e.recordEvent(r.ID, "completed", "Review completed")
	if err := e.app.ReactToIssueComment(ctx, token, r.RepoFull, r.TriggerCommentID, "rocket"); err != nil {
		log.Warn("acknowledge completed review", "cause", "github")
	}
	log.Info("review finished")
}

func (e *Engine) handleReviewError(ctx context.Context, token string, r *store.Review, log *slog.Logger, err error) {
	if !e.serviceLeaseOwned() {
		log.Info("service lease lost before review update")
		return
	}
	if errors.Is(err, store.ErrReviewLeaseLost) {
		log.Info("review lease lost before completion")
		return
	}
	if errors.Is(err, store.ErrPublicationReconcile) || (r.PublicationPrepared && (errors.Is(err, errPublicationInFlight) || errors.Is(err, store.ErrReviewNotReady))) {
		if reconcileErr := e.st.FinishReviewReconciliationRequired(r.ID, r.ExecutionGeneration, reconciliationReviewError, r.ServiceLeaseOwner, r.ClaimFence); reconcileErr != nil {
			if errors.Is(reconcileErr, store.ErrReviewLeaseLost) {
				log.Info("review lease lost before reconciliation state")
				return
			}
			log.Error("mark review reconciliation required", "err", reconcileErr)
		}
		return
	}
	if errors.Is(err, runner.ErrHeadChanged) {
		e.failHeadChanged(r, log)
		return
	}
	if errors.Is(err, errReviewAccess) {
		if e.failReview(r, log, "access_revoked", reviewAccessRevokedMessage) {
			e.signalReviewFailure(ctx, token, r, log)
		}
		return
	}
	if isRetryableReviewError(err) && r.ExecutionGeneration < maxDeliveryAttempts {
		delay := transientRetryDelay
		switch {
		case errors.Is(err, errPublicationInFlight):
			delay = store.OutboundHandoffTimeout + time.Second
		case errors.Is(err, runner.ErrQuota) && e.cfg.ZenCooldown > 0:
			delay = e.cfg.ZenCooldown
		}
		if requeueErr := e.st.RequeueReview(r.ID, r.ExecutionGeneration, delay, r.ServiceLeaseOwner, r.ClaimFence); requeueErr != nil {
			if errors.Is(requeueErr, store.ErrReviewLeaseLost) {
				log.Info("review lease lost before retry")
				return
			}
			log.Error("requeue review", "err", requeueErr)
			return
		}
		cause := reviewErrorClass(err, e.cfg.ReviewEngine)
		log.Warn("review delivery deferred", "attempt", r.ExecutionGeneration, "cause", cause)
		e.recordEvent(r.ID, "retry", fmt.Sprintf("Transient failure (%s); requeued for retry", cause))
		e.signal()
		return
	}
	// A cancelled worker context means shutdown or worker replacement, not a
	// review defect. Leave the durable running row alone: startup recovery
	// requeues it instead of failing the requester's review on every deploy.
	if errors.Is(err, context.Canceled) {
		log.Info("review interrupted; durable recovery will requeue it", "cause", reviewErrorClass(err, e.cfg.ReviewEngine))
		return
	}
	// Terminal failure: record the outcome so the dashboard and the PR's
	// active-review slot are released. Only the operator-owned error class is
	// stored; the underlying error can carry hostile repository content or
	// provider secrets. The sanitized runner diagnostic and GitHub endpoint
	// and status are safe for the service log.
	attrs := []any{"cause", reviewErrorClass(err, e.cfg.ReviewEngine)}
	if detail := runner.Diagnostic(err); detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	var httpErr *gh.HTTPError
	if errors.As(err, &httpErr) {
		attrs = append(attrs, "github_status", httpErr.StatusCode, "github_endpoint", httpErr.Method+" "+httpErr.Path)
	}
	log.Error("review failed", attrs...)
	cause := reviewErrorClass(err, e.cfg.ReviewEngine)
	if e.failReview(r, log, cause, fmt.Sprintf("%s (cause: %s)", reviewFailureMessage, cause)) {
		e.signalReviewFailure(ctx, token, r, log)
	}
}

// signalReviewFailure best-effort reacts to the trigger comment so the
// requester can see the review stopped, mirroring the success reaction.
func (e *Engine) signalReviewFailure(ctx context.Context, token string, r *store.Review, log *slog.Logger) {
	if e.app == nil || r == nil || r.TriggerCommentID < 1 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	if token == "" {
		acquired, err := e.app.InstallationToken(ctx, r.InstallationID, r.RepositoryID)
		if err != nil {
			log.Debug("skip failure reaction", "cause", "installation token unavailable")
			return
		}
		token = acquired
	}
	if err := e.app.ReactToIssueComment(ctx, token, r.RepoFull, r.TriggerCommentID, failureReaction); err != nil {
		log.Warn("acknowledge failed review", "cause", "github")
	}
}

func isRetryableReviewError(err error) bool {
	return gh.IsRetryable(err) || errors.Is(err, pool.ErrEmpty) || errors.Is(err, runner.ErrQuota) ||
		errors.Is(err, runner.ErrAborted) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, store.ErrReviewNotReady) ||
		errors.Is(err, errPublicationInFlight)
}

func reviewErrorClass(err error, engine string) string {
	switch {
	case errors.Is(err, runner.ErrCheckoutTooLarge):
		return "checkout_too_large"
	case errors.Is(err, runner.ErrCheckoutRejected):
		return "checkout_rejected"
	case errors.Is(err, runner.ErrQuota):
		return "provider_quota"
	case errors.Is(err, runner.ErrAborted):
		return "model_aborted"
	case errors.Is(err, runner.ErrExecution):
		if engine == config.EnginePi {
			return "pi_execution"
		}
		return "opencode_execution"
	case errors.Is(err, runner.ErrHeadChanged):
		return "head_changed"
	case errors.Is(err, runner.ErrOutputTooLarge):
		return "output_too_large"
	case errors.Is(err, runner.ErrSandboxUnavailable):
		return "sandbox_unavailable"
	case errors.Is(err, pool.ErrEmpty):
		return "no_key_available"
	case errors.Is(err, store.ErrReviewNotReady):
		return "review_not_ready"
	case errors.Is(err, errPublicationInFlight):
		return "publication_in_flight"
	case gh.IsRetryable(err):
		return "github_transient"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		var httpErr *gh.HTTPError
		if errors.As(err, &httpErr) {
			return fmt.Sprintf("github_http_%d", httpErr.StatusCode)
		}
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) {
			return fmt.Sprintf("sqlite_%d", sqliteErr.Code())
		}
		var netErr net.Error
		if errors.As(err, &netErr) {
			return "network_error"
		}
		return "unknown"
	}
}

func (e *Engine) failHeadChanged(r *store.Review, log *slog.Logger) {
	log.Info("review skipped because pull request head changed")
	e.failReview(r, log, "head_changed", headChangedMessage)
}

// uncertainDeliveryStatus reports whether a publication may already carry a
// remote effect whose acknowledgement never became durable. Pending
// publications were never POSTed, so discarding them is always safe.
func uncertainDeliveryStatus(status string) bool {
	return status == store.PublicationSending || status == store.PublicationReconciliationRequired
}

// handlePreparedHeadChanged reconciles effects from a prepared review before
// releasing the active-review constraint. A force-push must not strand a
// sending publication or let a replacement review race an unknown comment.
func (e *Engine) handlePreparedHeadChanged(ctx context.Context, token string, r *store.Review, log *slog.Logger) {
	if err := e.reconcilePreparedPublications(ctx, token, r); err != nil {
		e.handleReviewError(ctx, token, r, log, err)
		return
	}
	e.failHeadChanged(r, log)
}

func (e *Engine) reconcilePreparedPublications(ctx context.Context, token string, r *store.Review) error {
	publications, err := e.st.ListReviewPublications(r.ID)
	if err != nil {
		return errors.Join(store.ErrPublicationReconcile, err)
	}
	for _, publication := range publications {
		if !uncertainDeliveryStatus(publication.Status) {
			continue
		}
		if publication.Status == store.PublicationSending {
			claimed, claimErr := e.st.ClaimPublicationForSend(r.ID, r.ExecutionGeneration, publication.ID, r.ServiceLeaseOwner, r.ClaimFence)
			if errors.Is(claimErr, store.ErrPublicationReconcile) {
				// The stale handoff is now marker-only; continue with the
				// read-only reconciliation path below.
			} else if claimErr != nil {
				return errors.Join(store.ErrPublicationReconcile, claimErr)
			} else if !claimed {
				return errors.Join(store.ErrPublicationReconcile, errPublicationInFlight)
			}
		}
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return errors.Join(store.ErrPublicationReconcile, err)
		}
		commentID, found, err := e.findPublication(ctx, token, r, publication)
		if err != nil {
			return errors.Join(store.ErrPublicationReconcile, err)
		}
		if !found {
			return store.ErrPublicationReconcile
		}
		if err := e.reviewSideEffectError(ctx, r); err != nil {
			return errors.Join(store.ErrPublicationReconcile, err)
		}
		if err := e.st.MarkPublicationPosted(r.ID, r.ExecutionGeneration, publication.ID, commentID, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
			return errors.Join(store.ErrPublicationReconcile, err)
		}
	}
	return nil
}

func (e *Engine) reviewSideEffectError(ctx context.Context, r *store.Review) error {
	if err := e.processError(ctx); err != nil {
		return err
	}
	current, err := e.st.ReviewLeaseCurrent(r.ID, r.ExecutionGeneration, r.ServiceLeaseOwner, r.ClaimFence)
	if err != nil {
		return err
	}
	if !current {
		return store.ErrReviewLeaseLost
	}
	return nil
}

func (e *Engine) reviewCompletionSideEffectError(ctx context.Context, r *store.Review) error {
	if err := e.processError(ctx); err != nil {
		return err
	}
	current, err := e.st.ReviewCompletionLeaseCurrent(r.ID, r.ExecutionGeneration, r.ServiceLeaseOwner, r.ClaimFence)
	if err != nil {
		return err
	}
	if !current {
		return store.ErrReviewLeaseLost
	}
	return nil
}

// failReview records the terminal failure for the current execution
// generation and reports whether this worker's update landed.
func (e *Engine) failReview(r *store.Review, log *slog.Logger, cause, message string) bool {
	if !e.serviceLeaseOwned() {
		log.Info("service lease lost before failure update")
		return false
	}
	if err := e.st.FinishReviewFailed(r.ID, r.ExecutionGeneration, message, r.ServiceLeaseOwner, r.ClaimFence); err != nil {
		if errors.Is(err, store.ErrReviewLeaseLost) {
			log.Info("review lease lost before failure update")
			return false
		}
		log.Error("mark review failed", "err", err)
		return false
	}
	e.recordEvent(r.ID, "failed", fmt.Sprintf("Review failed (%s)", cause))
	return true
}

func (e *Engine) processClaimedNudge(ctx context.Context, n *store.Nudge) {
	if n == nil {
		return
	}
	if !e.mayProcess(ctx) {
		return
	}
	log := e.log.With("access_nudge", n.ID, "repo", n.RepoFull, "pr", n.PRNumber)
	if !e.cfg.AllowsReviewTarget(n.InstallationID, n.RepositoryID) {
		e.failNudge(n, log)
		return
	}
	if !e.mayProcess(ctx) {
		return
	}
	token, err := e.app.InstallationToken(ctx, n.InstallationID, n.RepositoryID)
	if err != nil {
		e.handleNudgeError(n, log, err)
		return
	}
	if !e.mayProcess(ctx) {
		return
	}
	pr, err := e.app.GetPR(ctx, token, n.RepoFull, n.PRNumber)
	if err != nil {
		e.handleNudgeError(n, log, err)
		return
	}
	verifiedRepo, err := verifyPRTarget(n.RepositoryID, n.PRNumber, pr)
	if err != nil {
		e.handleNudgeError(n, log, err)
		return
	}
	n.RepoFull = verifiedRepo
	if err := e.st.SetNudgeRepository(n.ID, n.ExecutionGeneration, verifiedRepo, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
		e.handleNudgeError(n, log, err)
		return
	}
	if !e.mayProcess(ctx) {
		return
	}
	marker := store.NudgeMarker(n.PublicationToken)
	commentID, found, err := e.app.FindIssueCommentMarker(ctx, token, n.RepoFull, n.PRNumber, marker)
	if err != nil {
		e.handleNudgeError(n, log, err)
		return
	}
	if n.ReconciliationRequired {
		if !found {
			if markErr := e.st.MarkNudgeReconciliationRequired(n.ID, n.ExecutionGeneration, n.ServiceLeaseOwner, n.ClaimFence); markErr != nil {
				log.Error("retain access notification reconciliation state", "err", markErr)
			}
			return
		}
		if err := e.st.MarkNudgePosted(n.ID, n.ExecutionGeneration, commentID, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
			e.handleNudgeError(n, log, err)
		}
		return
	}
	if !found {
		if !e.mayProcess(ctx) {
			return
		}
		// Re-read and verify the PR immediately before recording the send
		// handoff. A rename or force-push during marker lookup must not turn
		// this worker into a stale credentialed writer.
		finalPR, err := e.app.GetPR(ctx, token, n.RepoFull, n.PRNumber)
		if err != nil {
			e.handleNudgeError(n, log, err)
			return
		}
		finalRepo, err := verifyPRTarget(n.RepositoryID, n.PRNumber, finalPR)
		if err != nil {
			e.handleNudgeError(n, log, err)
			return
		}
		n.RepoFull = finalRepo
		if err := e.st.SetNudgeRepository(n.ID, n.ExecutionGeneration, finalRepo, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
			e.handleNudgeError(n, log, err)
			return
		}
		if err := e.st.MarkNudgeSending(n.ID, n.ExecutionGeneration, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
			e.handleNudgeError(n, log, err)
			return
		}
		n.SendingStartedAt = time.Now().UTC()
		current, leaseErr := e.st.NudgeLeaseCurrent(n.ID, n.ExecutionGeneration, n.ServiceLeaseOwner, n.ClaimFence)
		if leaseErr != nil || !current || !e.mayProcess(ctx) {
			e.handleNudgeError(n, log, context.Canceled)
			return
		}
		commentID, err = e.app.CreateIssueComment(ctx, token, n.RepoFull, n.PRNumber, nudgeBody(n, e.cfg)+"\n\n"+marker)
		if err != nil {
			e.handleNudgeError(n, log, err)
			return
		}
	}
	if !e.mayProcess(ctx) {
		e.handleNudgeError(n, log, context.Canceled)
		return
	}
	if err := e.st.MarkNudgePosted(n.ID, n.ExecutionGeneration, commentID, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
		if errors.Is(err, store.ErrNudgeLeaseLost) {
			log.Info("access notification lease lost before completion")
			return
		}
		e.handleNudgeError(n, log, err)
	}
}

func nudgeBody(n *store.Nudge, cfg *config.Config) string {
	if n.Kind == store.NudgeRegistration {
		return fmt.Sprintf("👋 Hi @%s — reviews are only available to registered users.\n\nPlease register at %s, then mention `@%s` again and I'll review this PR.",
			n.RequesterLogin, cfg.PublicURL, cfg.BotUsername)
	}
	return fmt.Sprintf("👋 Hi @%s — you are registered, but this shared review service requires an explicit access grant.\n\nPlease contact a service administrator, then mention `@%s` again.",
		n.RequesterLogin, cfg.BotUsername)
}

func verifyPRTarget(repositoryID, prNumber int64, pr *gh.PR) (string, error) {
	if repositoryID < 1 || pr == nil || pr.Number != prNumber || pr.TargetRepositoryID() != repositoryID {
		return "", errRepositoryIdentity
	}
	repo := strings.TrimSpace(pr.TargetRepositoryFullName())
	if !gh.ValidRepoFullName(repo) {
		return "", errRepositoryIdentity
	}
	return repo, nil
}

func (e *Engine) handleNudgeError(n *store.Nudge, log *slog.Logger, err error) {
	if !e.serviceLeaseOwned() {
		log.Info("service lease lost before access notification update")
		return
	}
	if errors.Is(err, store.ErrNudgeLeaseLost) {
		log.Info("access notification lease lost")
		return
	}
	if errors.Is(err, store.ErrNudgeReconcile) {
		log.Info("access notification requires reconciliation")
		return
	}
	if n.ReconciliationRequired || !n.SendingStartedAt.IsZero() {
		if reconcileErr := e.st.MarkNudgeReconciliationRequired(n.ID, n.ExecutionGeneration, n.ServiceLeaseOwner, n.ClaimFence); reconcileErr != nil {
			if errors.Is(reconcileErr, store.ErrNudgeLeaseLost) {
				log.Info("access notification lease lost before reconciliation state")
				return
			}
			log.Error("retain access notification reconciliation state", "err", reconcileErr)
		}
		return
	}
	if (gh.IsRetryable(err) || errors.Is(err, context.DeadlineExceeded)) && n.ExecutionGeneration < maxDeliveryAttempts {
		delay := transientRetryDelay
		if !n.SendingStartedAt.IsZero() {
			delay = store.OutboundHandoffTimeout + time.Second
		}
		if requeueErr := e.st.RequeueNudge(n.ID, n.ExecutionGeneration, delay, n.ServiceLeaseOwner, n.ClaimFence); requeueErr != nil {
			if errors.Is(requeueErr, store.ErrNudgeLeaseLost) {
				log.Info("access notification lease lost before retry")
				return
			}
			log.Error("requeue access notification", "err", requeueErr)
			return
		}
		log.Warn("access notification delivery deferred", "attempt", n.ExecutionGeneration)
		e.signal()
		return
	}
	e.failNudge(n, log)
}

func (e *Engine) failNudge(n *store.Nudge, log *slog.Logger) {
	if !e.serviceLeaseOwned() {
		log.Info("service lease lost before access notification failure update")
		return
	}
	if err := e.st.FinishNudgeFailed(n.ID, n.ExecutionGeneration, n.ServiceLeaseOwner, n.ClaimFence); err != nil {
		if errors.Is(err, store.ErrNudgeLeaseLost) {
			log.Info("access notification lease lost before failure update")
			return
		}
		log.Error("mark access notification failed", "err", err)
	}
}
