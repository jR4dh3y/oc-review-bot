package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

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

func toReviewJSON(r store.Review, findings int) reviewJSON {
	return reviewJSON{
		ID: r.ID, RepoFull: r.RepoFull, PRNumber: r.PRNumber, HeadSHA: r.HeadSHA,
		RequesterLogin: r.RequesterLogin, Status: r.Status, Model: r.Model,
		SummaryMD: r.SummaryMD, Error: r.Error, SummaryCommentID: r.SummaryCommentID,
		CreatedAt: r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), FindingsCount: findings,
	}
}

// handleReviews lists recent reviews with finding counts.
func (s *Server) handleReviews(u *store.User, w http.ResponseWriter, r *http.Request) {
	reviews, err := s.st.ListReviews(50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db failed")
		return
	}
	out := make([]reviewJSON, 0, len(reviews))
	for _, rev := range reviews {
		findings, _ := s.st.ListFindings(rev.ID)
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
		writeErr(w, http.StatusNotFound, "review not found")
		return
	}
	findings, err := s.st.ListFindings(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"review":   toReviewJSON(*rev, len(findings)),
		"findings": findings,
	})
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Secret == "" {
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Disabled == nil {
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model == "" {
		writeErr(w, http.StatusBadRequest, "model required")
		return
	}
	if err := s.st.SetSetting("model", body.Model); err != nil {
		writeErr(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "updated"})
}
