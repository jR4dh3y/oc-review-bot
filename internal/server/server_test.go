package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/bot"
	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/pool"
	"github.com/jR4dh3y/oc-review-bot/internal/seal"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

type testDeps struct {
	cfg *config.Config
	st  *store.Store
	app *gh.App
}

func setup(t *testing.T, ghHandler http.Handler) (*Server, *testDeps, *int, *[]string) {
	t.Helper()
	var commentCalls int
	var comments []string
	mux := http.NewServeMux()
	if ghHandler != nil {
		mux.Handle("/", ghHandler)
	}
	mux.HandleFunc("/repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		commentCalls++
		var body struct {
			Body string `json:"body"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		comments = append(comments, body.Body)
		w.Write([]byte(`{"id": 123}`))
	})
	mux.HandleFunc("/repos/o/r/issues/comments/99/reactions", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"t","expires_at":"2099-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/repos/o/r/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"number":7,"head":{"sha":"abc"},"base":{"sha":"def"}}`))
	})
	mux.HandleFunc("/repos/o/r/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	ghsrv := httptest.NewServer(mux)
	t.Cleanup(ghsrv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	t.Setenv("GITHUB_API_BASE", ghsrv.URL)
	app, err := gh.NewApp("1", pemText, "whsec")
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := seal.New("test-secret")
	st, err := store.Open(t.TempDir()+"/s.db", enc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Port: "8080", PublicURL: "http://localhost:8080",
		BotUsername: "oc-review-bot", DefaultModel: "opencode/big-pickle",
	}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	eng := bot.NewEngine(cfg, st, app, pool.New(st, time.Hour), log)
	s := &Server{cfg: cfg, st: st, app: app, engine: eng, log: log}
	return s, &testDeps{cfg: cfg, st: st, app: app}, &commentCalls, &comments
}

func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func commentPayload(action, body, senderType string, hasPR bool) []byte {
	ev := map[string]any{
		"action":  action,
		"comment": map[string]any{"id": 99, "body": body},
		"issue":   map[string]any{"number": 7},
		"repository": map[string]any{
			"full_name": "o/r",
		},
		"installation": map[string]any{"id": 1},
		"sender":       map[string]any{"login": "Alice", "type": senderType},
	}
	if hasPR {
		ev["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "x"}
	}
	b, _ := json.Marshal(ev)
	return b
}

func postWebhook(t *testing.T, h http.Handler, app *gh.App, event string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(t, "whsec", payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookBadSignature(t *testing.T) {
	s, _, _, _ := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(`{}`))
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestWebhookPing(t *testing.T) {
	s, _, _, _ := setup(t, nil)
	rec := postWebhook(t, New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{}), s.app, "ping", []byte(`{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestUnregisteredUserGetsNudge(t *testing.T) {
	s, deps, calls, comments := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := postWebhook(t, h, s.app, "issue_comment", commentPayload("created", "@oc-review-bot review", "User", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("nudge comments = %d, want 1", *calls)
	}
	if !strings.Contains((*comments)[0], "register") {
		t.Fatalf("nudge body = %q", (*comments)[0])
	}
	// No review created for unregistered users.
	if _, err := deps.st.ActiveReview("o/r", 7); err != store.ErrNotFound {
		t.Fatal("review should not be created for unregistered user")
	}

	// Second nudge on the same PR is suppressed.
	rec = postWebhook(t, h, s.app, "issue_comment", commentPayload("created", "@oc-review-bot review", "User", true))
	if rec.Code != http.StatusOK || *calls != 1 {
		t.Fatalf("duplicate nudge posted: code=%d calls=%d", rec.Code, *calls)
	}
}

func TestRegisteredUserEnqueuesReview(t *testing.T) {
	s, deps, _, _ := setup(t, nil)
	deps.st.UpsertUser(10, "alice", "", false)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := postWebhook(t, h, s.app, "issue_comment", commentPayload("created", "please @OC-REVIEW-BOT review this", "User", true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	active, err := deps.st.ActiveReview("o/r", 7)
	if err != nil || active.RequesterLogin != "alice" {
		t.Fatalf("active review = %+v, %v", active, err)
	}

	// Duplicate mention while queued is ignored.
	rec = postWebhook(t, h, s.app, "issue_comment", commentPayload("created", "@oc-review-bot again", "User", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("dedupe code = %d", rec.Code)
	}
}

func TestIgnoresNonMentionsAndBots(t *testing.T) {
	s, deps, calls, _ := setup(t, nil)
	deps.st.UpsertUser(10, "alice", "", false)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"no mention", commentPayload("created", "looks good, ship it", "User", true)},
		{"bot sender", commentPayload("created", "@oc-review-bot review", "Bot", true)},
		{"not a PR", commentPayload("created", "@oc-review-bot review", "User", false)},
		{"deleted action", commentPayload("deleted", "@oc-review-bot review", "User", true)},
	} {
		rec := postWebhook(t, h, s.app, "issue_comment", tc.payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code = %d", tc.name, rec.Code)
		}
	}
	if *calls != 0 {
		t.Fatalf("unexpected comments: %d", *calls)
	}
	if _, err := deps.st.ActiveReview("o/r", 7); err != store.ErrNotFound {
		t.Fatal("no review should be created")
	}
}

func TestAdminAPIKeysRoundTrip(t *testing.T) {
	s, _, _, _ := setup(t, nil)
	s.st.UpsertUser(1, "root", "", true)
	admin, _ := s.st.UpsertUser(2, "pleb", "", false)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	loginAs := func(u *store.User) (*httptest.ResponseRecorder, *http.Request) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		tok := "tok-" + u.Login
		s.st.CreateSession(u.ID, tok, time.Hour)
		req.AddCookie(&http.Cookie{Name: "oc_review_session", Value: tok})
		return httptest.NewRecorder(), req
	}

	// Non-admin is forbidden.
	rec, req := loginAs(admin)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d", rec.Code)
	}

	// Admin adds, lists, disables, deletes.
	addReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"label":"k1","secret":"sk-secret-1234"}`))
	tok := "tok-root"
	s.st.CreateSession(1, tok, time.Hour)
	addReq.AddCookie(&http.Cookie{Name: "oc_review_session", Value: tok})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, addReq)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add code = %d, %s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	getReq.AddCookie(&http.Cookie{Name: "oc_review_session", Value: tok})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, getReq)
	var keys []map[string]any
	json.NewDecoder(rec.Body).Decode(&keys)
	if len(keys) != 1 || keys[0]["last4"] != "1234" {
		t.Fatalf("keys = %v", keys)
	}
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatal("secret leaked in list response")
	}
}
