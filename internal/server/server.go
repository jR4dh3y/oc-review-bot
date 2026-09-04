// Package server wires HTTP routes: webhooks, GitHub OAuth, the JSON API
// for the dashboard, and the embedded SPA.
package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/bot"
	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

// Server holds shared dependencies for the HTTP handlers.
type Server struct {
	cfg    *config.Config
	st     *store.Store
	app    *gh.App
	engine *bot.Engine
	log    *slog.Logger
}

// New builds the HTTP router. spa is the embedded web/dist tree (nil in dev).
func New(cfg *config.Config, st *store.Store, app *gh.App, eng *bot.Engine, log *slog.Logger, spa embed.FS) http.Handler {
	s := &Server{cfg: cfg, st: st, app: app, engine: eng, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /webhooks/github", s.handleWebhook)
	mux.HandleFunc("GET /auth/github/login", s.handleOAuthLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleOAuthCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/me", s.withUser(s.handleMe))
	mux.HandleFunc("GET /api/reviews", s.withUser(s.handleReviews))
	mux.HandleFunc("GET /api/reviews/{id}", s.withUser(s.handleReviewDetail))
	mux.HandleFunc("GET /api/admin/keys", s.withAdmin(s.handleListKeys))
	mux.HandleFunc("POST /api/admin/keys", s.withAdmin(s.handleAddKey))
	mux.HandleFunc("PATCH /api/admin/keys/{id}", s.withAdmin(s.handleKeyPatch))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.withAdmin(s.handleDeleteKey))
	mux.HandleFunc("GET /api/admin/settings", s.withAdmin(s.handleGetSettings))
	mux.HandleFunc("POST /api/admin/settings", s.withAdmin(s.handleSetSettings))

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
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
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
				http.ServeContent(w, r, "index.html", time.Now(), mustIndex(subtree))
			})
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "oc-review-bot API — the web UI is embedded at build time.")
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

const sessionCookie = "oc_review_session"

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

func (s *Server) currentUser(r *http.Request) (*store.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	u, err := s.st.SessionUser(c.Value)
	if err != nil {
		return nil, false
	}
	return u, true
}

func (s *Server) withUser(next func(*store.User, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		next(u, w, r)
	}
}

func (s *Server) withAdmin(next func(*store.User, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.withUser(func(u *store.User, w http.ResponseWriter, r *http.Request) {
		if !u.IsAdmin {
			writeErr(w, http.StatusForbidden, "admin required")
			return
		}
		next(u, w, r)
	})
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var _ = context.Background
