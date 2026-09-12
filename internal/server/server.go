// Package server wires HTTP routes: webhooks, GitHub OAuth, the JSON API
// for the dashboard, and the embedded SPA.
package server

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jR4dh3y/samik-bot/internal/bot"
	"github.com/jR4dh3y/samik-bot/internal/config"
	"github.com/jR4dh3y/samik-bot/internal/gh"
	"github.com/jR4dh3y/samik-bot/internal/orca"
	"github.com/jR4dh3y/samik-bot/internal/store"
)

// Server holds shared dependencies for the HTTP handlers.
type Server struct {
	cfg    *config.Config
	st     *store.Store
	app    *gh.App
	engine *bot.Engine
	log    *slog.Logger
	orca   *orca.Service
}

// New builds the HTTP router. spa is the embedded web/dist tree (nil in dev).
func New(cfg *config.Config, st *store.Store, app *gh.App, eng *bot.Engine, log *slog.Logger, spa embed.FS) http.Handler {
	s := &Server{
		cfg:    cfg,
		st:     st,
		app:    app,
		engine: eng,
		log:    log,
		orca:   orca.NewService(cfg.BotUsername, cfg.PublicURL+"/auth/orca/callback", cfg.OrcaReferralCode),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /webhooks/github", s.handleWebhook)
	mux.HandleFunc("GET /auth/github/login", s.handleOAuthLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleOAuthCallback)
	mux.HandleFunc("GET /auth/orca/callback", s.handleOrcaCallback)
	mux.HandleFunc("POST /auth/logout", s.withTrustedOrigin(s.handleLogout))
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/me", s.withUser(s.handleMe))
	mux.HandleFunc("GET /api/reviews", s.withUser(s.handleReviews))
	mux.HandleFunc("GET /api/reviews/{id}", s.withUser(s.handleReviewDetail))
	mux.HandleFunc("GET /api/admin/keys", s.withAdmin(s.handleListKeys))
	mux.HandleFunc("POST /api/admin/keys", s.withAdminMutation(s.handleAddKey))
	mux.HandleFunc("PATCH /api/admin/keys/{id}", s.withAdminMutation(s.handleKeyPatch))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.withAdminMutation(s.handleDeleteKey))
	mux.HandleFunc("GET /api/admin/settings", s.withAdmin(s.handleGetSettings))
	mux.HandleFunc("POST /api/admin/settings", s.withAdminMutation(s.handleSetSettings))
	mux.HandleFunc("GET /api/admin/partner", s.withAdmin(s.handlePartnerInfo))
	mux.HandleFunc("GET /orca/connect-url", s.withAdmin(s.handleOrcaConnectURL))
	mux.Handle("GET /", spaHandler(spa))
	return withLogging(log, mux)
}

func withLogging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).String())
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.st == nil || s.engine == nil || !s.engine.Ready() || s.st.Ping(r.Context()) != nil {
		writeErr(w, http.StatusServiceUnavailable, "service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// spaHandler serves the embedded dist build with SPA fallback, or a stub in dev.
func spaHandler(spa embed.FS) http.Handler {
	if spa != (embed.FS{}) {
		if subtree, err := fs.Sub(spa, "dist"); err == nil {
			fileServer := http.FileServer(http.FS(subtree))
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") ||
					strings.HasPrefix(r.URL.Path, "/webhooks/") || r.URL.Path == "/healthz" {
					http.NotFound(w, r)
					return
				}
				path := strings.TrimPrefix(r.URL.Path, "/")
				if path != "" {
					if f, err := subtree.Open(path); err == nil {
						f.Close()
						fileServer.ServeHTTP(w, r)
						return
					}
				}
				// index.html references content-hashed assets, so it must always
				// revalidate: a cached document would pin browsers to an old bundle
				// after every new binary.
				w.Header().Set("Cache-Control", "no-store")
				http.ServeContent(w, r, "index.html", time.Now(), mustIndex(subtree))
			})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "samik-bot API — the web UI is embedded at build time.")
	})
}

func mustIndex(subtree fs.FS) io.ReadSeeker {
	b, err := fs.ReadFile(subtree, "index.html")
	if err != nil {
		return strings.NewReader("")
	}
	return bytesReader(b)
}

func bytesReader(b []byte) io.ReadSeeker {
	return &byteSeeker{b: b}
}

type byteSeeker struct {
	b   []byte
	pos int64
}

func (s *byteSeeker) Read(p []byte) (int, error) {
	if s.pos >= int64(len(s.b)) {
		return 0, io.EOF
	}
	n := copy(p, s.b[s.pos:])
	s.pos += int64(n)
	return n, nil
}

func (s *byteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	case io.SeekEnd:
		s.pos = int64(len(s.b)) + offset
	}
	return s.pos, nil
}

// sessionCookie helpers.

const (
	sessionCookie     = "samik_session"
	hostSessionCookie = "__Host-samik_session"
	oauthStateCookie  = "oc_oauth_state"
	hostOAuthCookie   = "__Host-oc_oauth_state"
)

var (
	errDuplicateAuthCookie       = errors.New("duplicate authentication cookie")
	errAuthenticationUnavailable = errors.New("authentication dependencies unavailable")
)

const requestBodyTimeout = 15 * time.Second

func setRequestBodyDeadline(w http.ResponseWriter) {
	// The production net/http server also enforces ReadTimeout; this shorter
	// deadline covers handlers exercised behind a compatible ResponseWriter.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(requestBodyTimeout))
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

func (s *Server) currentUser(r *http.Request) (*store.User, error) {
	c, err := uniqueRequestCookie(r, s.sessionCookieName())
	if err != nil || c == nil || c.Value == "" {
		return nil, store.ErrNotFound
	}
	if s.st == nil || s.cfg == nil {
		return nil, errAuthenticationUnavailable
	}
	u, err := s.st.SessionUser(c.Value)
	if err != nil {
		return nil, err
	}
	// Database flags are only cached display data. Configuration is the current
	// immutable source of truth, so removed admins lose access immediately.
	u.IsAdmin = s.cfg.IsAdminGitHubID(u.GitHubID)
	return u, nil
}

func (s *Server) withUser(next func(*store.User, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.currentUser(r)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		if err != nil {
			s.log.Error("load current user", "err", err)
			writeErr(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		next(u, w, r)
	}
}

func (s *Server) withAdmin(next func(*store.User, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.withUser(func(u *store.User, w http.ResponseWriter, r *http.Request) {
		if !s.cfg.IsAdminGitHubID(u.GitHubID) {
			writeErr(w, http.StatusForbidden, "admin required")
			return
		}
		next(u, w, r)
	})
}

// withAdminMutation protects cookie-authenticated JSON mutations from
// cross-origin and simple-form requests. GitHub webhooks and OAuth are not
// browser session mutations and intentionally do not use this middleware.
func (s *Server) withAdminMutation(next func(*store.User, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.withAdmin(func(u *store.User, w http.ResponseWriter, r *http.Request) {
		setRequestBodyDeadline(w)
		if !s.hasTrustedOrigin(r) {
			writeErr(w, http.StatusForbidden, "invalid request origin")
			return
		}
		if !isJSONContentType(r) {
			writeErr(w, http.StatusUnsupportedMediaType, "application/json content type required")
			return
		}
		next(u, w, r)
	})
}

func (s *Server) withTrustedOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasTrustedOrigin(r) {
			writeErr(w, http.StatusForbidden, "invalid request origin")
			return
		}
		next(w, r)
	}
}

func (s *Server) hasTrustedOrigin(r *http.Request) bool {
	if s.cfg == nil {
		return false
	}
	origin, err := url.Parse(strings.TrimSpace(r.Header.Get("Origin")))
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil ||
		origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	publicURL, err := url.Parse(s.cfg.PublicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(origin.Scheme, publicURL.Scheme) && strings.EqualFold(origin.Host, publicURL.Host)
}

func isJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func (s *Server) secureCookies() bool {
	if s.cfg == nil {
		return false
	}
	if s.cfg.CookieSecure {
		return true
	}
	publicURL, err := url.Parse(s.cfg.PublicURL)
	return err == nil && strings.EqualFold(publicURL.Scheme, "https")
}

func (s *Server) sessionCookieName() string {
	if s.secureCookies() {
		return hostSessionCookie
	}
	return sessionCookie
}

func (s *Server) oauthStateCookieName() string {
	if s.secureCookies() {
		return hostOAuthCookie
	}
	return oauthStateCookie
}

// uniqueRequestCookie rejects cookie tossing and malformed duplicate headers
// instead of silently trusting net/http's first matching cookie.
func uniqueRequestCookie(r *http.Request, name string) (*http.Cookie, error) {
	var found *http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name != name {
			continue
		}
		if found != nil {
			return nil, errDuplicateAuthCookie
		}
		clone := *cookie
		found = &clone
	}
	return found, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
