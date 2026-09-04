// Package bot runs queued reviews end to end: GitHub fetch, agent run,
// finding mapping, and comment posting.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/pool"
	"github.com/jR4dh3y/oc-review-bot/internal/review"
	"github.com/jR4dh3y/oc-review-bot/internal/runner"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

// Engine consumes queued review IDs and drives the review pipeline.
type Engine struct {
	cfg  *config.Config
	st   *store.Store
	app  *gh.App
	pool *pool.Pool
	log  *slog.Logger
	jobs chan int64
}

func NewEngine(cfg *config.Config, st *store.Store, app *gh.App, p *pool.Pool, log *slog.Logger) *Engine {
	return &Engine{
		cfg: cfg, st: st, app: app, pool: p, log: log,
		jobs: make(chan int64, 256),
	}
}

// Start launches n background workers. Call Enqueue to submit work.
func (e *Engine) Start(n int) {
	for i := 0; i < n; i++ {
		go e.worker()
	}
}

func (e *Engine) Enqueue(reviewID int64) {
	e.jobs <- reviewID
}

func (e *Engine) worker() {
	for id := range e.jobs {
		ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ReviewTimeout)
		e.processOne(ctx, id)
		cancel()
	}
}

func (e *Engine) processOne(ctx context.Context, id int64) {
	r, err := e.st.Review(id)
	if err != nil {
		e.log.Error("load review", "id", id, "err", err)
		return
	}
	log := e.log.With("review", id, "repo", r.RepoFull, "pr", r.PRNumber)

	token, err := e.app.InstallationToken(ctx, r.InstallationID)
	if err != nil {
		e.fail(ctx, r, log, "get installation token: "+err.Error())
		return
	}

	pr, err := e.app.GetPR(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.fail(ctx, r, log, "get PR: "+err.Error())
		return
	}
	e.st.SetHeadSHA(r.ID, pr.Head.SHA)

	files, err := e.app.ListFiles(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.fail(ctx, r, log, "list PR files: "+err.Error())
		return
	}
	diff, err := e.app.GetDiff(ctx, token, r.RepoFull, r.PRNumber)
	if err != nil {
		e.fail(ctx, r, log, "get PR diff: "+err.Error())
		return
	}

	model := e.st.GetSetting("model", e.cfg.DefaultModel)
	key, agentOut, err := e.runWithPool(ctx, token, r, model, diff, log)
	if err != nil {
		e.fail(ctx, r, log, err.Error())
		return
	}
	_ = key // usage already recorded at acquire; kept for logging if needed

	result := review.ExtractReview(agentOut)
	idx := review.NewDiffIndex(files)
	postErr := e.postComments(ctx, token, r, pr, idx, result)
	if postErr != nil {
		e.fail(ctx, r, log, "post comments: "+postErr.Error())
		return
	}

	e.st.FinishReviewDone(r.ID, result.SummaryMD, r.SummaryCommentID)
	e.app.ReactToIssueComment(ctx, token, r.RepoFull, r.TriggerCommentID, "rocket")
	log.Info("review finished", "findings", len(result.Findings))
}

// runWithPool runs the agent, retrying once on a quota failure with the next
// key. It marks the review running with the model and key used.
func (e *Engine) runWithPool(ctx context.Context, token string, r *store.Review, model string, diff []byte, log *slog.Logger) (*store.ZenKey, string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		key, err := e.pool.Acquire()
		if err != nil {
			return nil, "", fmt.Errorf("acquire key: %w", err)
		}
		out, runErr := runner.Run(ctx, runner.Options{
			Bin:      e.cfg.OpenCodeBin,
			PreArgs:  e.cfg.OpenCodeArgs,
			CloneURL: fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, r.RepoFull),
			Ref:      fmt.Sprintf("refs/pull/%d/head", r.PRNumber),
			Model:    model,
			APIKey:   key.Secret,
			Diff:     diff,
			Prompt:   review.BuildPrompt("review-diff.patch"),
		})
		if runErr == nil {
			e.st.StartReview(r.ID, model, key.ID)
			return key, out, nil
		}
		if !errors.Is(runErr, runner.ErrQuota) {
			e.st.StartReview(r.ID, model, key.ID)
			return key, "", runErr
		}
		// Key exhausted: cool it down and try the next one.
		e.pool.Exhausted(key.ID)
		log.Warn("zen key exhausted, cooling down", "key_id", key.ID)
		lastErr = runErr
	}
	return nil, "", lastErr
}

// postComments writes inline findings and the summary comment, updating the
// review row with the summary comment id.
func (e *Engine) postComments(ctx context.Context, token string, r *store.Review, pr *gh.PR, idx *review.DiffIndex, result review.ReviewResult) error {
	for _, f := range result.Findings {
		if !idx.InDiff(f.Path, f.Side, f.Line) {
			continue
		}
		commentID, err := e.app.CreateReviewComment(ctx, token, r.RepoFull, r.PRNumber, gh.ReviewComment{
			CommitID: pr.Head.SHA,
			Path:     f.Path,
			Side:     f.Side,
			Line:     f.Line,
			Body:     review.RenderInlineBody(f),
		})
		if err != nil {
			return fmt.Errorf("inline comment on %s:%d: %w", f.Path, f.Line, err)
		}
		e.st.SaveFinding(&store.Finding{
			ReviewID: r.ID, Path: f.Path, Line: f.Line, Side: f.Side,
			Severity: f.Severity, BodyMD: f.Body, PostedCommentID: commentID,
		})
	}

	summary := review.RenderSummaryComment(result, e.cfg.BotUsername, e.modelUsed(r))
	commentID, err := e.app.CreateIssueComment(ctx, token, r.RepoFull, r.PRNumber, summary)
	if err != nil {
		return err
	}
	r.SummaryCommentID = commentID
	return nil
}

// modelUsed reports the model recorded for the review (set at run start).
func (e *Engine) modelUsed(r *store.Review) string {
	if fresh, err := e.st.Review(r.ID); err == nil && fresh.Model != "" {
		return fresh.Model
	}
	return e.st.GetSetting("model", e.cfg.DefaultModel)
}

// fail marks the review failed, posts a short comment on the PR, and reacts.
func (e *Engine) fail(ctx context.Context, r *store.Review, log *slog.Logger, msg string) {
	log.Error("review failed", "err", msg)
	e.st.FinishReviewFailed(r.ID, msg)
	if token, err := e.app.InstallationToken(ctx, r.InstallationID); err == nil {
		body := fmt.Sprintf("## 🤖 Review failed\n\n%s", msg)
		if len(body) > 2000 {
			body = body[:2000]
		}
		if _, cerr := e.app.CreateIssueComment(ctx, token, r.RepoFull, r.PRNumber, body); cerr != nil {
			log.Error("post failure comment", "err", cerr)
		}
		e.app.ReactToIssueComment(ctx, token, r.RepoFull, r.TriggerCommentID, "eyes") // neutral; no rocket
	}
}

// Trigger reacts with 👀 so the requester sees the bot picked the request up.
func (e *Engine) Trigger(ctx context.Context, installationID int64, repo string, commentID int64) {
	token, err := e.app.InstallationToken(ctx, installationID)
	if err != nil {
		e.log.Error("trigger reaction token", "err", err)
		return
	}
	if err := e.app.ReactToIssueComment(ctx, token, repo, commentID, "eyes"); err != nil {
		e.log.Error("trigger reaction", "err", err)
	}
}
