package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleOAuthLogin redirects to GitHub's OAuth authorize page.
func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.OAuthConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "github oauth is not configured")
		return
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{
		Name:     "oc_oauth_state",
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})
	url := fmt.Sprintf("https://github.com/login/oauth/authorize?client_id=%s&redirect_uri=%s/auth/github/callback&scope=read:user%%20user:email&state=%s",
		s.cfg.OAuthClientID, s.cfg.PublicURL, state)
	http.Redirect(w, r, url, http.StatusFound)
}

// handleOAuthCallback exchanges the code, upserts the user, and sets a session.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.OAuthConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "github oauth is not configured")
		return
	}
	stateCookie, err := r.Cookie("oc_oauth_state")
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		writeErr(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "oc_oauth_state", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing oauth code")
		return
	}
	ctx := r.Context()
	accessToken, err := s.app.ExchangeOAuthCode(ctx, s.cfg.OAuthClientID, s.cfg.OAuthClientSecret, code,
		s.cfg.PublicURL+"/auth/github/callback")
	if err != nil {
		s.log.Error("oauth exchange", "err", err)
		writeErr(w, http.StatusBadGateway, "github login failed")
		return
	}
	ghu, err := s.app.GetOAuthUser(ctx, accessToken)
	if err != nil {
		s.log.Error("oauth user", "err", err)
		writeErr(w, http.StatusBadGateway, "github login failed")
		return
	}

	forceAdmin := false
	for _, login := range s.cfg.AdminLogins {
		if strings.EqualFold(login, ghu.Login) {
			forceAdmin = true
			break
		}
	}
	user, err := s.st.UpsertUser(ghu.ID, ghu.Login, ghu.AvatarURL, forceAdmin)
	if err != nil {
		s.log.Error("upsert user", "err", err)
		writeErr(w, http.StatusInternalServerError, "login failed")
		return
	}

	sessionToken := randomHex(32)
	if err := s.st.CreateSession(user.ID, sessionToken, 30*24*time.Hour); err != nil {
		writeErr(w, http.StatusInternalServerError, "login failed")
		return
	}
	s.setSession(w, sessionToken)
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

// handleLogout clears the session cookie and row.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"ok": "logged out"})
}
