package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jR4dh3y/samik-bot/internal/store"
)

const maxWebhookBytes = 2 << 20

// GitHub webhook event payloads (only the fields we use).

type issueCommentEvent struct {
	Action  string `json:"action"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Issue struct {
		Number      int64 `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	} `json:"issue"`
	Repo struct {
		FullName string `json:"full_name"`
		ID       int64  `json:"id"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
		ID    int64  `json:"id"`
	} `json:"sender"`
}

// handleWebhook verifies the payload and dispatches supported events.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setRequestBodyDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBytes)
	payload, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes+1))
	if err != nil || len(payload) > maxWebhookBytes {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.app.VerifySignature(payload, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "pong"})
		return
	}
	if event != "issue_comment" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignored event")
		return
	}

	var ev issueCommentEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.Action != "created" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignored action")
		return
	}
	s.handleIssueComment(w, &ev, strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")))
}

// handleIssueComment processes a mention of the bot on a PR.
func (s *Server) handleIssueComment(w http.ResponseWriter, ev *issueCommentEvent, deliveryID string) {
	if ev.Issue.PullRequest == nil || ev.Issue.Number == 0 {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "not a pull request")
		return
	}
	if !validIssueCommentIdentity(ev) {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	// Do not retain deliveries or send access nudges for installations and
	// repositories outside the operator-owned review boundary.
	if !s.cfg.AllowsReviewTarget(ev.Installation.ID, ev.Repo.ID) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignored target")
		return
	}
	if !strings.EqualFold(ev.Sender.Type, "User") {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignoring non-human comment")
		return
	}
	if !mentionsBot(ev.Comment.Body, s.cfg.BotUsername) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "no mention")
		return
	}
	if deliveryID == "" {
		http.Error(w, "missing delivery", http.StatusBadRequest)
		return
	}

	login := strings.ToLower(ev.Sender.Login)
	repo := ev.Repo.FullName
	pr := ev.Issue.Number

	// Registration gate: only registered website users get reviews.
	if _, err := s.st.UserByGitHubID(ev.Sender.ID); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("load webhook user", "github_id", ev.Sender.ID, "err", err)
			http.Error(w, "db failed", http.StatusInternalServerError)
			return
		}
		s.queueNudge(w, ev, deliveryID, store.NudgeRegistration)
		return
	}
	if !s.cfg.CanRequestReview(ev.Sender.ID, ev.Installation.ID, ev.Repo.ID) {
		s.queueNudge(w, ev, deliveryID, store.NudgeEntitlement)
		return
	}

	rev := &store.Review{
		RepoFull:          repo,
		RepositoryID:      ev.Repo.ID,
		PRNumber:          pr,
		InstallationID:    ev.Installation.ID,
		RequesterGitHubID: ev.Sender.ID,
		RequesterLogin:    login,
		TriggerCommentID:  ev.Comment.ID,
	}
	result, err := s.st.CreateReviewWithAdmission(rev, deliveryID, store.ReviewAdmission{
		UserPerHour: s.cfg.UserReviewsPerHour,
		RepoPerHour: s.cfg.RepoReviewsPerHour,
		MaxActive:   s.cfg.MaxActiveReviews,
	})
	if err != nil {
		s.log.Error("create review", "err", err)
		http.Error(w, "db failed", http.StatusInternalServerError)
		return
	}
	if result.DuplicateDelivery {
		s.log.Info("duplicate webhook delivery, skipping", "delivery", deliveryID)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "duplicate delivery")
		return
	}
	if !result.Created {
		if result.RateLimited || result.QueueFull {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "review capacity unavailable")
			return
		}
		s.log.Info("review already active, skipping", "repo", repo, "pr", pr)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "review already active")
		return
	}

	s.engine.Enqueue(rev.ID)
	s.log.Info("review enqueued", "review", rev.ID, "repo", repo, "pr", pr, "by", login)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "review enqueued")
}

func (s *Server) queueNudge(w http.ResponseWriter, ev *issueCommentEvent, deliveryID, kind string) {
	nudge := &store.Nudge{
		RepoFull:          ev.Repo.FullName,
		RepositoryID:      ev.Repo.ID,
		PRNumber:          ev.Issue.Number,
		InstallationID:    ev.Installation.ID,
		RequesterGitHubID: ev.Sender.ID,
		RequesterLogin:    strings.ToLower(ev.Sender.Login),
		Kind:              kind,
	}
	result, err := s.st.CreateNudgeOnce(nudge, deliveryID)
	if err != nil {
		s.log.Error("create access nudge", "err", err)
		http.Error(w, "db failed", http.StatusInternalServerError)
		return
	}
	if result.DuplicateDelivery {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "duplicate delivery")
		return
	}
	if result.Created {
		s.engine.EnqueueNudge(nudge.ID)
	}
	w.WriteHeader(http.StatusOK)
	if kind == store.NudgeRegistration {
		fmt.Fprintln(w, "registration required")
		return
	}
	fmt.Fprintln(w, "review access required")
}

// mentionsBot reports whether body mentions @BotUsername (case-insensitive).
func mentionsBot(body, username string) bool {
	body = strings.ToLower(body)
	target := "@" + strings.ToLower(username)
	for offset := 0; ; {
		match := strings.Index(body[offset:], target)
		if match < 0 {
			return false
		}
		start := offset + match
		end := start + len(target)
		beforeOK := start == 0 || !isGitHubLoginByte(body[start-1])
		afterOK := end == len(body) || !isGitHubLoginByte(body[end])
		if beforeOK && afterOK {
			return true
		}
		offset = end
	}
}

func validIssueCommentIdentity(ev *issueCommentEvent) bool {
	return ev != nil && ev.Comment.ID > 0 && ev.Issue.Number > 0 && ev.Repo.ID > 0 && ev.Installation.ID > 0 &&
		ev.Sender.ID > 0 && validGitHubLogin(ev.Sender.Login) && validRepoFullName(ev.Repo.FullName)
}

func validGitHubLogin(login string) bool {
	if len(login) == 0 || len(login) > 39 || login[0] == '-' || login[len(login)-1] == '-' {
		return false
	}
	for i := range login {
		if !isGitHubLoginByte(login[i]) {
			return false
		}
	}
	return true
}

func validRepoFullName(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[0]) > 39 || len(parts[1]) == 0 || len(parts[1]) > 100 {
		return false
	}
	for _, part := range parts {
		for i := range part {
			c := part[i]
			if !(isGitHubLoginByte(c) || c == '.' || c == '_') {
				return false
			}
		}
	}
	return true
}

func isGitHubLoginByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-'
}
