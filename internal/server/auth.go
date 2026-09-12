package server

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jR4dh3y/samik-bot/internal/store"
)

const oauthStateLifetime = 10 * time.Minute

// handleOAuthLogin redirects to GitHub's OAuth authorize page.
func (s *Server) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg == nil || s.st == nil {
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	if !s.cfg.OAuthConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "github oauth is not configured")
		return
	}
	clientIP, err := requestClientIP(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid client address")
		return
	}
	state, err := randomHex(16)
	if err != nil {
		s.log.Error("generate oauth state", "err", err)
		writeErr(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	if err := s.st.CreateOAuthStateForClient(state, clientIP, oauthStateLifetime); err != nil {
		if errors.Is(err, store.ErrOAuthStateLimit) || errors.Is(err, store.ErrOAuthStateClientLimit) {
			writeErr(w, http.StatusTooManyRequests, "too many active login attempts")
			return
		}
		s.log.Error("persist oauth state", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.oauthStateCookieName(),
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateLifetime.Seconds()),
	})
	authorizeURL := &url.URL{Scheme: "https", Host: "github.com", Path: "/login/oauth/authorize"}
	query := authorizeURL.Query()
	query.Set("client_id", s.cfg.OAuthClientID)
	query.Set("redirect_uri", s.cfg.PublicURL+"/auth/github/callback")
	query.Set("scope", "read:user")
	query.Set("state", state)
	authorizeURL.RawQuery = query.Encode()
	http.Redirect(w, r, authorizeURL.String(), http.StatusFound)
}

// handleOAuthCallback exchanges the code, upserts the user, and sets a session.
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg == nil || s.st == nil || s.app == nil {
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	if !s.cfg.OAuthConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "github oauth is not configured")
		return
	}
	stateCookie, cookieErr := uniqueRequestCookie(r, s.oauthStateCookieName())
	state := r.URL.Query().Get("state")
	if cookieErr != nil || stateCookie == nil || stateCookie.Value == "" || state == "" ||
		subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(state)) != 1 {
		s.clearOAuthState(w)
		writeErr(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	consumed, err := s.st.ConsumeOAuthState(state)
	if err != nil {
		s.log.Error("consume oauth state", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	if !consumed {
		s.clearOAuthState(w)
		writeErr(w, http.StatusBadRequest, "invalid oauth state")
		return
	}
	s.clearOAuthState(w)

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

	user, err := s.st.UpsertUser(ghu.ID, ghu.Login, ghu.AvatarURL, s.cfg.IsAdminGitHubID(ghu.ID))
	if err != nil {
		s.log.Error("upsert user", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}

	sessionToken, err := randomHex(32)
	if err != nil {
		s.log.Error("generate session token", "err", err)
		writeErr(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	if err := s.st.CreateSession(user.ID, sessionToken, 30*24*time.Hour); err != nil {
		s.log.Error("create session", "err", err)
		writeErr(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	s.setSession(w, sessionToken)
	// The marker lets the SPA notify already-open tabs after the OAuth redirect.
	http.Redirect(w, r, "/dashboard?auth=oauth", http.StatusFound)
}

// handleLogout clears the session cookie and row.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, err := uniqueRequestCookie(r, s.sessionCookieName())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session cookie")
		return
	}
	oauthState, err := uniqueRequestCookie(r, s.oauthStateCookieName())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid oauth state cookie")
		return
	}
	if s.st == nil && (session != nil || oauthState != nil) {
		writeErr(w, http.StatusServiceUnavailable, "logout unavailable")
		return
	}
	if session != nil || oauthState != nil {
		sessionToken := ""
		if session != nil {
			sessionToken = session.Value
		}
		stateToken := ""
		if oauthState != nil {
			stateToken = oauthState.Value
		}
		if err := s.st.DeleteAuthSessionAndOAuthState(sessionToken, stateToken); err != nil {
			s.log.Error("revoke logout credentials", "err", err)
			// Keep both cookies so a transient store failure does not falsely claim
			// that a copied session token was revoked.
			writeErr(w, http.StatusServiceUnavailable, "logout unavailable")
			return
		}
	}
	s.clearSession(w)
	s.clearOAuthState(w)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "logged out"})
}

func requestClientIP(r *http.Request) (string, error) {
	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if remoteAddr == "" {
		return "", errors.New("missing remote address")
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", err
	}
	// Do not trust forwarding headers without an explicit trusted-proxy policy.
	return address.Unmap().String(), nil
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Path:     "/",
		Secure:   s.secureCookies(),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (s *Server) clearOAuthState(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.oauthStateCookieName(),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}
