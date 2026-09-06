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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
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

const (
	testRepoFull             = "o/r"
	testPRNumber       int64 = 7
	testInstallationID int64 = 101
	testRepositoryID   int64 = 202
	testRequesterID    int64 = 303
	testAdminID        int64 = 404
	testBobID          int64 = 505
)

func setup(t *testing.T, ghHandler http.Handler) (*Server, *testDeps, *int) {
	t.Helper()
	var commentCalls int
	mux := http.NewServeMux()
	if ghHandler != nil {
		mux.Handle("/", ghHandler)
	}
	mux.HandleFunc("/repos/"+testRepoFull+"/issues/"+strconv.FormatInt(testPRNumber, 10)+"/comments", func(w http.ResponseWriter, r *http.Request) {
		commentCalls++
		w.Write([]byte(`{"id": 123}`))
	})
	mux.HandleFunc("/repos/"+testRepoFull+"/issues/comments/99/reactions", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/app/installations/"+strconv.FormatInt(testInstallationID, 10)+"/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&struct{}{})
		w.Write([]byte(`{"token":"t","expires_at":"2099-01-01T00:00:00Z"}`))
	})
	mux.HandleFunc("/repos/"+testRepoFull+"/pulls/"+strconv.FormatInt(testPRNumber, 10), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"number":7,"head":{"sha":"abc"},"base":{"sha":"def","repo":{"id":202,"full_name":"o/r"}}}`))
	})
	mux.HandleFunc("/repos/"+testRepoFull+"/pulls/"+strconv.FormatInt(testPRNumber, 10)+"/files", func(w http.ResponseWriter, r *http.Request) {
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
	app, err := gh.NewApp("1", pemText, "whsec", 707)
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
		Port:              "8080",
		PublicURL:         "http://localhost:8080",
		AdminGitHubIDs:    []int64{testAdminID},
		ReviewerGitHubIDs: []int64{testRequesterID},
		InstallationIDs:   []int64{testInstallationID},
		RepositoryIDs:     []int64{testRepositoryID},
		BotUsername:       "oc-review-bot",
		DefaultModel:      "opencode/big-pickle",
	}
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	eng := bot.NewEngine(cfg, st, app, pool.New(st, time.Hour), log)
	s := &Server{cfg: cfg, st: st, app: app, engine: eng, log: log}
	return s, &testDeps{cfg: cfg, st: st, app: app}, &commentCalls
}

func claimNextQueuedNudge(t *testing.T, st *store.Store) (*store.Nudge, error) {
	t.Helper()
	const owner = "server-test-owner"
	lease, acquired, err := st.AcquireServiceLeaseWithFence(owner, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		var current bool
		lease, current, err = st.CurrentServiceLease(owner)
		if err != nil || !current {
			t.Fatalf("read current test lease = %+v, %v, %v", lease, current, err)
		}
		if lease.Fence < 1 {
			t.Fatalf("current test lease has invalid fence: %+v", lease)
		}
	}
	return st.ClaimNextQueuedNudge(owner, lease.Fence)
}

func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func commentPayload(action, body, senderType string, hasPR bool) []byte {
	return commentPayloadForTarget(action, body, senderType, hasPR, testInstallationID, testRepositoryID)
}

func commentPayloadForTarget(action, body, senderType string, hasPR bool, installationID, repositoryID int64) []byte {
	ev := map[string]any{
		"action":  action,
		"comment": map[string]any{"id": 99, "body": body},
		"issue":   map[string]any{"number": testPRNumber},
		"repository": map[string]any{
			"full_name": testRepoFull,
			"id":        repositoryID,
		},
		"installation": map[string]any{"id": installationID},
		"sender": map[string]any{
			"id": testRequesterID, "login": "Alice", "type": senderType,
		},
	}
	if hasPR {
		ev["issue"].(map[string]any)["pull_request"] = map[string]any{"url": "x"}
	}
	b, _ := json.Marshal(ev)
	return b
}

var webhookDeliverySequence int64

func postWebhook(t *testing.T, h http.Handler, event string, payload []byte) *httptest.ResponseRecorder {
	t.Helper()
	return postWebhookWithDelivery(t, h, event, payload,
		"test-delivery-"+strconv.FormatInt(atomic.AddInt64(&webhookDeliverySequence, 1), 10))
}

func postWebhookWithDelivery(t *testing.T, h http.Handler, event string, payload []byte, deliveryID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(t, "whsec", payload))
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookBadSignature(t *testing.T) {
	s, _, _ := setup(t, nil)
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
	s, _, _ := setup(t, nil)
	rec := postWebhook(t, New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{}), "ping", []byte(`{}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestMetaReturnsConfiguredBotUsernameWithoutSession(t *testing.T) {
	s, _, _ := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusOK)
	}
	var body struct {
		BotUsername string `json:"bot_username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.BotUsername != s.cfg.BotUsername || body.BotUsername == "" {
		t.Fatalf("bot_username = %q, want configured %q", body.BotUsername, s.cfg.BotUsername)
	}
}

func TestHealthIsUnavailableBeforeWorkerStart(t *testing.T) {
	s, _, _ := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestUnregisteredUserQueuesNudgeWithoutSynchronousComment(t *testing.T) {
	s, deps, calls := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := postWebhook(t, h, "issue_comment", commentPayload("created", "@oc-review-bot review", "User", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if *calls != 0 {
		t.Fatalf("nudge comments = %d, want 0 before a worker runs", *calls)
	}
	// No review created for unregistered users.
	if _, err := deps.st.ActiveReview(testRepositoryID, testPRNumber); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("review should not be created for unregistered user: %v", err)
	}

	// A distinct delivery for the same requester and PR cannot create another nudge.
	rec = postWebhook(t, h, "issue_comment", commentPayload("created", "@oc-review-bot review", "User", true))
	if rec.Code != http.StatusOK || *calls != 0 {
		t.Fatalf("duplicate nudge queued a synchronous comment: code=%d calls=%d", rec.Code, *calls)
	}
	nudge, err := claimNextQueuedNudge(t, deps.st)
	if err != nil {
		t.Fatalf("claim durable nudge: %v", err)
	}
	if nudge.RepoFull != testRepoFull || nudge.RepositoryID != testRepositoryID || nudge.PRNumber != testPRNumber ||
		nudge.InstallationID != testInstallationID || nudge.RequesterGitHubID != testRequesterID ||
		nudge.RequesterLogin != "alice" || nudge.Kind != store.NudgeRegistration || nudge.Status != store.NudgePosting ||
		nudge.PublicationToken == "" {
		t.Fatalf("queued nudge = %+v", nudge)
	}
	if _, err := claimNextQueuedNudge(t, deps.st); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("duplicate nudge was persisted: %v", err)
	}
}

func TestRegisteredUserEnqueuesReview(t *testing.T) {
	s, deps, calls := setup(t, nil)
	if _, err := deps.st.UpsertUser(testRequesterID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	rec := postWebhook(t, h, "issue_comment", commentPayload("created", "please @OC-REVIEW-BOT review this", "User", true))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	active, err := deps.st.ActiveReview(testRepositoryID, testPRNumber)
	if err != nil {
		t.Fatal(err)
	}
	if active.RepoFull != testRepoFull || active.RepositoryID != testRepositoryID || active.PRNumber != testPRNumber ||
		active.InstallationID != testInstallationID || active.RequesterGitHubID != testRequesterID ||
		active.RequesterLogin != "alice" || active.TriggerCommentID != 99 || active.Status != store.StatusQueued {
		t.Fatalf("queued review = %+v", active)
	}

	// Duplicate mention while queued is ignored.
	rec = postWebhook(t, h, "issue_comment", commentPayload("created", "@oc-review-bot again", "User", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("dedupe code = %d", rec.Code)
	}
	reviews, err := deps.st.ListReviews(10)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
	if *calls != 0 {
		t.Fatalf("review submission made a synchronous GitHub comment: %d", *calls)
	}
}

func TestWebhookDeliveryDoesNotReplayCompletedReview(t *testing.T) {
	s, deps, _ := setup(t, nil)
	if _, err := deps.st.UpsertUser(testRequesterID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	payload := commentPayload("created", "@oc-review-bot review", "User", true)

	rec := postWebhookWithDelivery(t, h, "issue_comment", payload, "delivery-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first code = %d, body = %s", rec.Code, rec.Body.String())
	}
	active, err := deps.st.ActiveReview(testRepositoryID, testPRNumber)
	if err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := deps.st.AcquireServiceLeaseWithFence("server-review-owner", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire review lease = %+v, %v, %v", lease, acquired, err)
	}
	if err := deps.st.StartReview(active.ID, "model", 1, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	running, err := deps.st.Review(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.st.PrepareReviewPublication(running.ID, running.ExecutionGeneration, store.PublicationPlan{
		SummaryMD: "done", SummaryBodyMD: "done", CommitSHA: "abc",
	}, running.ServiceLeaseOwner, running.ClaimFence); err != nil {
		t.Fatal(err)
	}
	publications, err := deps.st.ListReviewPublications(running.ID)
	if err != nil || len(publications) != 1 || publications[0].Kind != store.PublicationSummary {
		t.Fatalf("publications = %+v, %v", publications, err)
	}
	if ok, err := deps.st.ClaimPublicationForSend(running.ID, running.ExecutionGeneration, publications[0].ID, running.ServiceLeaseOwner, running.ClaimFence); err != nil || !ok {
		t.Fatalf("claim publication = %v, %v", ok, err)
	}
	if err := deps.st.MarkPublicationPosted(running.ID, running.ExecutionGeneration, publications[0].ID, 123, running.ServiceLeaseOwner, running.ClaimFence); err != nil {
		t.Fatal(err)
	}
	if err := deps.st.FinishReviewDone(running.ID, running.ExecutionGeneration, running.ServiceLeaseOwner, running.ClaimFence); err != nil {
		t.Fatal(err)
	}

	rec = postWebhookWithDelivery(t, h, "issue_comment", payload, "delivery-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay code = %d, body = %s", rec.Code, rec.Body.String())
	}
	reviews, err := deps.st.ListReviews(10)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
}

func TestWebhookDeliveryCannotStartReviewAfterRegistration(t *testing.T) {
	s, deps, _ := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	payload := commentPayload("created", "@oc-review-bot review", "User", true)

	rec := postWebhookWithDelivery(t, h, "issue_comment", payload, "delivery-before-registration")
	if rec.Code != http.StatusOK {
		t.Fatalf("unregistered code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := deps.st.UpsertUser(testRequesterID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	rec = postWebhookWithDelivery(t, h, "issue_comment", payload, "delivery-before-registration")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := deps.st.ActiveReview(testRepositoryID, testPRNumber); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replayed delivery created review: %v", err)
	}
}

func TestIgnoresNonMentionsAndBots(t *testing.T) {
	s, deps, calls := setup(t, nil)
	if _, err := deps.st.UpsertUser(testRequesterID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
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
		rec := postWebhook(t, h, "issue_comment", tc.payload)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code = %d", tc.name, rec.Code)
		}
	}
	if *calls != 0 {
		t.Fatalf("unexpected comments: %d", *calls)
	}
	if _, err := deps.st.ActiveReview(testRepositoryID, testPRNumber); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no review should be created: %v", err)
	}
	if _, err := claimNextQueuedNudge(t, deps.st); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ignored event queued a nudge: %v", err)
	}
}

func TestWebhookIgnoresDisallowedTargetsWithoutPersistingWork(t *testing.T) {
	s, deps, calls := setup(t, nil)
	if _, err := deps.st.UpsertUser(testRequesterID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	for _, tc := range []struct {
		name           string
		installationID int64
		repositoryID   int64
	}{
		{"installation", testInstallationID + 1, testRepositoryID},
		{"repository", testInstallationID, testRepositoryID + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postWebhookWithDelivery(t, h, "issue_comment",
				commentPayloadForTarget("created", "@oc-review-bot review", "User", true, tc.installationID, tc.repositoryID),
				"disallowed-"+tc.name)
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if *calls != 0 {
		t.Fatalf("disallowed targets made GitHub comments: %d", *calls)
	}
	reviews, err := deps.st.ListReviews(10)
	if err != nil || len(reviews) != 0 {
		t.Fatalf("disallowed targets persisted reviews: %+v, %v", reviews, err)
	}
	if _, err := claimNextQueuedNudge(t, deps.st); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disallowed targets persisted nudges: %v", err)
	}
}

func TestAdminAPIKeysRoundTrip(t *testing.T) {
	s, _, _ := setup(t, nil)
	admin, err := s.st.UpsertUser(testAdminID, "root", "", true)
	if err != nil {
		t.Fatal(err)
	}
	nonAdmin, err := s.st.UpsertUser(testRequesterID, "pleb", "", false)
	if err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	loginAs := func(u *store.User) (*httptest.ResponseRecorder, *http.Request) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
		tok := "tok-" + u.Login
		s.st.CreateSession(u.ID, tok, time.Hour)
		req.AddCookie(&http.Cookie{Name: "oc_review_session", Value: tok})
		return httptest.NewRecorder(), req
	}

	// Non-admin is forbidden.
	rec, req := loginAs(nonAdmin)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin code = %d", rec.Code)
	}

	// Admin adds, lists, disables, deletes.
	addReq := httptest.NewRequest(http.MethodPost, "/api/admin/keys",
		strings.NewReader(`{"label":"k1","secret":"sk-secret-1234"}`))
	addReq.Header.Set("Origin", s.cfg.PublicURL)
	addReq.Header.Set("Content-Type", "application/json")
	tok := "tok-root"
	s.st.CreateSession(admin.ID, tok, time.Hour)
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

func TestCurrentUserRejectsDuplicateSessionCookies(t *testing.T) {
	s, deps, _ := setup(t, nil)
	u, err := deps.st.UpsertUser(testRequesterID, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.st.CreateSession(u.ID, "duplicate-cookie-session", time.Hour); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "duplicate-cookie-session"})
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "duplicate-cookie-session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate session cookie code = %d", rec.Code)
	}
}

func TestCurrentUserDistinguishesInvalidSessionsFromStoreFailures(t *testing.T) {
	t.Run("invalid session", func(t *testing.T) {
		s, _, _ := setup(t, nil)
		h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "unknown-session-token"})
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("invalid session code = %d, want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("store failure", func(t *testing.T) {
		s, _, _ := setup(t, nil)
		h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
		if err := s.st.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session-token"})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("store failure code = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestOAuthLoginLimitsAttemptsPerClientIP(t *testing.T) {
	s, _, _ := setup(t, nil)
	s.cfg.OAuthClientID = "client-id"
	s.cfg.OAuthClientSecret = "client-secret"
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	requestFor := func(remoteAddr string, forwardedFor string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/auth/github/login", nil)
		req.RemoteAddr = remoteAddr
		req.Header.Set("X-Forwarded-For", forwardedFor)
		return req
	}
	limited := false
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, requestFor("192.0.2.10:1234", "198.51.100."+strconv.Itoa(i+1)))
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("login %d code = %d, want %d", i, rec.Code, http.StatusFound)
		}
	}
	if !limited {
		t.Fatal("same client was never limited")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor("192.0.2.11:1234", "203.0.113.1"))
	if rec.Code != http.StatusFound {
		t.Fatalf("different client code = %d, want %d", rec.Code, http.StatusFound)
	}
}

func TestSecureCookiesUseHostPrefix(t *testing.T) {
	s, _, _ := setup(t, nil)
	s.cfg.CookieSecure = true

	rec := httptest.NewRecorder()
	s.setSession(rec, "session-token")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hostSessionCookie || !cookies[0].Secure || cookies[0].Path != "/" || cookies[0].Domain != "" {
		t.Fatalf("session cookie = %+v", cookies)
	}

	rec = httptest.NewRecorder()
	s.clearOAuthState(rec)
	cookies = rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != hostOAuthCookie || !cookies[0].Secure || cookies[0].Path != "/" || cookies[0].Domain != "" {
		t.Fatalf("oauth cookie = %+v", cookies)
	}
}

func TestSecureCookiesRecognizeCaseInsensitivePublicURLScheme(t *testing.T) {
	s, _, _ := setup(t, nil)
	s.cfg.CookieSecure = false
	s.cfg.PublicURL = "HTTPS://example.test"

	if !s.secureCookies() {
		t.Fatal("HTTPS PublicURL should require secure cookies regardless of scheme casing")
	}
	if name := s.sessionCookieName(); name != hostSessionCookie {
		t.Fatalf("session cookie name = %q, want %q", name, hostSessionCookie)
	}
}

func TestLogoutPreservesCookieWhenSessionDeletionFails(t *testing.T) {
	s, _, _ := setup(t, nil)
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	if err := s.st.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Origin", s.cfg.PublicURL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "still-valid-if-not-revoked"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout failure code = %d", rec.Code)
	}
	if value := rec.Header().Get("Set-Cookie"); value != "" {
		t.Fatalf("logout failure cleared cookie: %q", value)
	}
}

func TestLogoutClearsPendingOAuthState(t *testing.T) {
	s, deps, _ := setup(t, nil)
	user, err := deps.st.UpsertUser(testRequesterID, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	const sessionToken = "session-token"
	const oauthState = "pending-oauth-state"
	if err := deps.st.CreateSession(user.ID, sessionToken, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := deps.st.CreateOAuthStateForClient(oauthState, "192.0.2.10", time.Hour); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Origin", s.cfg.PublicURL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: oauthState})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if consumed, err := deps.st.ConsumeOAuthState(oauthState); err != nil || consumed {
		t.Fatalf("pending oauth state after logout = consumed:%t err:%v", consumed, err)
	}
	if _, err := deps.st.SessionUser(sessionToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session after logout error = %v, want %v", err, store.ErrNotFound)
	}
	cleared := map[string]bool{}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.MaxAge < 0 {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[sessionCookie] || !cleared[oauthStateCookie] {
		t.Fatalf("logout cookies = %+v, want cleared session and oauth state", cleared)
	}
}

func TestLogoutClearsPendingOAuthStateWithoutSession(t *testing.T) {
	s, deps, _ := setup(t, nil)
	const oauthState = "pending-oauth-state-without-session"
	if err := deps.st.CreateOAuthStateForClient(oauthState, "192.0.2.10", time.Hour); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.Header.Set("Origin", s.cfg.PublicURL)
	req.AddCookie(&http.Cookie{Name: oauthStateCookie, Value: oauthState})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if consumed, err := deps.st.ConsumeOAuthState(oauthState); err != nil || consumed {
		t.Fatalf("pending oauth state after logout = consumed:%t err:%v", consumed, err)
	}
}

func TestReviewsFilterUnauthorizedHistoryBeforeLimiting(t *testing.T) {
	s, deps, _ := setup(t, nil)
	alice, err := deps.st.UpsertUser(testRequesterID, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := deps.st.UpsertUser(testAdminID, "root", "", true)
	if err != nil {
		t.Fatal(err)
	}
	visible := &store.Review{
		RepoFull: "o/r", RepositoryID: testRepositoryID, PRNumber: 1, InstallationID: testInstallationID,
		RequesterGitHubID: testRequesterID, RequesterLogin: "alice", TriggerCommentID: 1,
	}
	if err := deps.st.CreateReview(visible); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		historical := &store.Review{
			RepoFull: "archived/r", RepositoryID: testRepositoryID + 1, PRNumber: int64(i + 10), InstallationID: testInstallationID,
			RequesterGitHubID: testRequesterID, RequesterLogin: "alice", TriggerCommentID: int64(i + 10),
		}
		if err := deps.st.CreateReview(historical); err != nil {
			t.Fatalf("create inaccessible history %d: %v", i, err)
		}
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	for _, user := range []*store.User{alice, admin} {
		t.Run(user.Login, func(t *testing.T) {
			token := "history-" + user.Login
			if err := deps.st.CreateSession(user.ID, token, time.Hour); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/reviews", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("list code = %d, body = %s", rec.Code, rec.Body.String())
			}
			var reviews []reviewJSON
			if err := json.NewDecoder(rec.Body).Decode(&reviews); err != nil {
				t.Fatal(err)
			}
			if len(reviews) != 1 || reviews[0].ID != visible.ID {
				t.Fatalf("visible reviews = %+v", reviews)
			}
		})
	}
}

func TestReviewsAreScopedToRequesterUnlessAdmin(t *testing.T) {
	s, deps, _ := setup(t, nil)
	alice, err := deps.st.UpsertUser(testRequesterID, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deps.st.UpsertUser(testBobID, "bob", "", false); err != nil {
		t.Fatal(err)
	}
	admin, err := deps.st.UpsertUser(testAdminID, "root", "", true)
	if err != nil {
		t.Fatal(err)
	}
	aliceReview := &store.Review{
		RepoFull: "o/r", RepositoryID: testRepositoryID, PRNumber: 1, InstallationID: testInstallationID,
		RequesterGitHubID: testRequesterID, RequesterLogin: "alice", TriggerCommentID: 1,
	}
	bobReview := &store.Review{
		RepoFull: "o/r", RepositoryID: testRepositoryID, PRNumber: 2, InstallationID: testInstallationID,
		RequesterGitHubID: testBobID, RequesterLogin: "bob", TriggerCommentID: 2,
	}
	if err := deps.st.CreateReview(aliceReview); err != nil {
		t.Fatal(err)
	}
	if err := deps.st.CreateReview(bobReview); err != nil {
		t.Fatal(err)
	}
	h := New(s.cfg, s.st, s.app, s.engine, s.log, embed.FS{})

	sessions := map[int64]string{}
	requestFor := func(method, path string, user *store.User) *http.Request {
		tok, ok := sessions[user.ID]
		if !ok {
			tok = "reviews-" + user.Login
			if err := deps.st.CreateSession(user.ID, tok, time.Hour); err != nil {
				t.Fatal(err)
			}
			sessions[user.ID] = tok
		}
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		return req
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(http.MethodGet, "/api/reviews", alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice list code = %d", rec.Code)
	}
	var aliceReviews []reviewJSON
	if err := json.NewDecoder(rec.Body).Decode(&aliceReviews); err != nil {
		t.Fatal(err)
	}
	if len(aliceReviews) != 1 || aliceReviews[0].ID != aliceReview.ID {
		t.Fatalf("alice reviews = %+v", aliceReviews)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(http.MethodGet, "/api/reviews/"+strconv.FormatInt(bobReview.ID, 10), alice))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("alice foreign detail code = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(http.MethodGet, "/api/reviews/"+strconv.FormatInt(aliceReview.ID, 10), alice))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice own detail code = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(http.MethodGet, "/api/reviews", admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list code = %d", rec.Code)
	}
	var adminReviews []reviewJSON
	if err := json.NewDecoder(rec.Body).Decode(&adminReviews); err != nil {
		t.Fatal(err)
	}
	if len(adminReviews) != 2 {
		t.Fatalf("admin reviews = %+v", adminReviews)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(http.MethodGet, "/api/reviews/"+strconv.FormatInt(bobReview.ID, 10), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin foreign detail code = %d", rec.Code)
	}
}
