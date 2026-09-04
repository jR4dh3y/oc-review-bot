package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

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
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender"`
}

// handleWebhook verifies the payload and dispatches supported events.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
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
	s.handleIssueComment(r.Context(), w, &ev)
}

// handleIssueComment processes a mention of the bot on a PR.
func (s *Server) handleIssueComment(ctx context.Context, w http.ResponseWriter, ev *issueCommentEvent) {
	if ev.Issue.PullRequest == nil || ev.Issue.Number == 0 {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "not a pull request")
		return
	}
	if ev.Sender.Type == "Bot" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ignoring bot comment")
		return
	}
	if !mentionsBot(ev.Comment.Body, s.cfg.BotUsername) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "no mention")
		return
	}

	login := strings.ToLower(ev.Sender.Login)
	repo := ev.Repo.FullName
	pr := ev.Issue.Number

	// Registration gate: only registered website users get reviews.
	if _, err := s.st.UserByLogin(login); err != nil {
		s.nudgeRegister(ctx, ev, login, repo, pr)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "registration required")
		return
	}

	// Dedupe: one active review per PR.
	if active, err := s.st.ActiveReview(repo, pr); err == nil {
		s.log.Info("review already active, skipping", "review", active.ID, "repo", repo, "pr", pr)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "review already active")
		return
	}

	token, err := s.app.InstallationToken(ctx, ev.Installation.ID)
	if err != nil {
		s.log.Error("installation token", "err", err)
		http.Error(w, "github auth failed", http.StatusBadGateway)
		return
	}
	prInfo, err := s.app.GetPR(ctx, token, repo, pr)
	if err != nil {
		s.log.Error("get PR", "err", err)
		http.Error(w, "github api failed", http.StatusBadGateway)
		return
	}

	rev := &store.Review{
		RepoFull:         repo,
		PRNumber:         pr,
		HeadSHA:          prInfo.Head.SHA,
		InstallationID:   ev.Installation.ID,
		RequesterLogin:   login,
		TriggerCommentID: ev.Comment.ID,
	}
	if err := s.st.CreateReview(rev); err != nil {
		s.log.Error("create review", "err", err)
		http.Error(w, "db failed", http.StatusInternalServerError)
		return
	}

	s.engine.Trigger(ctx, ev.Installation.ID, repo, ev.Comment.ID)
	s.engine.Enqueue(rev.ID)
	s.log.Info("review enqueued", "review", rev.ID, "repo", repo, "pr", pr, "by", login)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintln(w, "review enqueued")
}

// nudgeRegister tells an unregistered commenter to register, once per PR.
func (s *Server) nudgeRegister(ctx context.Context, ev *issueCommentEvent, login, repo string, pr int64) {
	posted, err := s.st.NudgePosted(repo, pr, login)
	if err != nil || posted {
		return
	}
	token, err := s.app.InstallationToken(ctx, ev.Installation.ID)
	if err != nil {
		s.log.Error("nudge token", "err", err)
		return
	}
	body := fmt.Sprintf("👋 Hi @%s — reviews are only available to registered users.\n\n"+
		"Please register at %s, then mention `@%s` again and I'll review this PR.",
		login, s.cfg.PublicURL, s.cfg.BotUsername)
	if _, err := s.app.CreateIssueComment(ctx, token, repo, pr, body); err != nil {
		s.log.Error("nudge comment", "err", err)
		return
	}
	s.st.MarkNudge(repo, pr, login)
}

// mentionsBot reports whether body mentions @BotUsername (case-insensitive).
func mentionsBot(body, username string) bool {
	return strings.Contains(strings.ToLower(body), "@"+strings.ToLower(username))
}
