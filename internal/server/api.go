package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jR4dh3y/samik-bot/internal/config"
	"github.com/jR4dh3y/samik-bot/internal/store"
)

const maxAPIJSONBytes = 64 << 10

// decodeJSONBody accepts exactly one bounded JSON object after the route's
// middleware has verified its content type and origin.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIJSONBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

// handleMe returns the current user.
func (s *Server) handleMe(u *store.User, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "login": u.Login, "avatar_url": u.AvatarURL, "is_admin": u.IsAdmin,
	})
}

type reviewJSON struct {
	ID               int64  `json:"id"`
	RepoFull         string `json:"repo_full"`
	PRNumber         int64  `json:"pr_number"`
	HeadSHA          string `json:"head_sha"`
	RequesterLogin   string `json:"requester_login"`
	Status           string `json:"status"`
	Model            string `json:"model"`
	SummaryMD        string `json:"summary_md"`
	Error            string `json:"error"`
	SummaryCommentID int64  `json:"summary_comment_id"`
	CreatedAt        string `json:"created_at"`
	FindingsCount    int    `json:"findings_count"`
}

type findingJSON struct {
	ID              int64  `json:"id"`
	ReviewID        int64  `json:"review_id"`
	Path            string `json:"path"`
	Line            int64  `json:"line"`
	Side            string `json:"side"`
	Severity        string `json:"severity"`
	Body            string `json:"body"`
	PostedCommentID int64  `json:"posted_comment_id"`
}

func toReviewJSON(r store.Review, findings int) reviewJSON {
	return reviewJSON{
		ID: r.ID, RepoFull: r.RepoFull, PRNumber: r.PRNumber, HeadSHA: r.HeadSHA,
		RequesterLogin: r.RequesterLogin, Status: r.Status, Model: r.Model,
		SummaryMD: r.SummaryMD, Error: r.Error, SummaryCommentID: r.SummaryCommentID,
		CreatedAt: r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), FindingsCount: findings,
	}
}

func toFindingJSON(f store.Finding) findingJSON {
	return findingJSON{
		ID: f.ID, ReviewID: f.ReviewID, Path: f.Path, Line: f.Line, Side: f.Side,
		Severity: f.Severity, Body: f.BodyMD, PostedCommentID: f.PostedCommentID,
	}
}

// handleReviews lists recent reviews with finding counts.
func (s *Server) handleReviews(u *store.User, w http.ResponseWriter, r *http.Request) {
	var (
		reviews []store.Review
		err     error
	)
	if u.IsAdmin {
		reviews, err = s.st.ListReviewsForTargets(s.cfg.InstallationIDs, s.cfg.RepositoryIDs, 50)
	} else {
		reviews, err = s.st.ListReviewsForRequesterGitHubIDAndTargets(
			u.GitHubID, s.cfg.InstallationIDs, s.cfg.RepositoryIDs, 50,
		)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db failed")
		return
	}
	out := make([]reviewJSON, 0, len(reviews))
	for _, rev := range reviews {
		if !s.canViewReview(u, rev) {
			continue
		}
		findings, err := s.st.ListFindings(rev.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db failed")
			return
		}
		out = append(out, toReviewJSON(rev, len(findings)))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleReviewDetail returns one review with its findings.
func (s *Server) handleReviewDetail(u *store.User, w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	rev, err := s.st.Review(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "review not found")
		} else {
			writeErr(w, http.StatusInternalServerError, "db failed")
		}
		return
	}
	if !s.canViewReview(u, *rev) {
		// Return the same response as a missing review to avoid exposing IDs.
		writeErr(w, http.StatusNotFound, "review not found")
		return
	}
	findings, err := s.st.ListFindings(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db failed")
		return
	}
	publicFindings := make([]findingJSON, 0, len(findings))
	for _, finding := range findings {
		publicFindings = append(publicFindings, toFindingJSON(finding))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"review":   toReviewJSON(*rev, len(findings)),
		"findings": publicFindings,
	})
}

// Historical review data is retained for auditability but is visible only
// while the requester still has the current immutable review entitlement.
func (s *Server) canViewReview(u *store.User, rev store.Review) bool {
	if u == nil || !s.cfg.AllowsReviewTarget(rev.InstallationID, rev.RepositoryID) {
		return false
	}
	if u.IsAdmin {
		return true
	}
	return rev.RequesterGitHubID == u.GitHubID && s.cfg.CanRequestReview(u.GitHubID, rev.InstallationID, rev.RepositoryID)
}

// handleListKeys lists masked Zen keys.
func (s *Server) handleListKeys(u *store.User, w http.ResponseWriter, r *http.Request) {
	keys, err := s.st.ListKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db failed")
		return
	}
	if keys == nil {
		keys = []store.MaskedZenKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleAddKey stores a new Zen key.
func (s *Server) handleAddKey(u *store.User, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label  string `json:"label"`
		Secret string `json:"secret"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil || body.Secret == "" {
		writeErr(w, http.StatusBadRequest, "label and secret required")
		return
	}
	if body.Label == "" {
		body.Label = "zen-key"
	}
	k, err := s.st.AddKey(body.Label, body.Secret)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	s.log.Info("zen key added", "id", k.ID, "label", k.Label)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": k.ID, "label": k.Label, "last4": k.Last4,
	})
}

// handleKeyPatch toggles a key disabled.
func (s *Server) handleKeyPatch(u *store.User, w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil || body.Disabled == nil {
		writeErr(w, http.StatusBadRequest, "disabled required")
		return
	}
	if err := s.st.DisableKey(id, *body.Disabled); err != nil {
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "updated"})
}

// handleDeleteKey removes a key.
func (s *Server) handleDeleteKey(u *store.User, w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct{}
	if err := decodeJSONBody(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "empty JSON object required")
		return
	}
	if err := s.st.DeleteKey(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

// handleGetSettings returns admin settings (currently the default model).
func (s *Server) handleGetSettings(u *store.User, w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"model": s.st.GetSetting("model", s.cfg.DefaultModel),
	})
}

// handleSetSettings updates admin settings.
func (s *Server) handleSetSettings(u *store.User, w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil || !config.ValidModel(body.Model) {
		writeErr(w, http.StatusBadRequest, "model required")
		return
	}
	if err := s.st.SetSetting("model", body.Model); err != nil {
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "updated"})
}

// handleMeta exposes unauthenticated public branding: the mentionable bot
// login the landing page and dashboard tell users to mention. The login is
// already public in every posted nudge comment, so this needs no session.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"bot_username": s.cfg.BotUsername,
	})
}
