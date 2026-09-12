package bot

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jR4dh3y/samik-bot/internal/config"
	"github.com/jR4dh3y/samik-bot/internal/gh"
	"github.com/jR4dh3y/samik-bot/internal/pool"
	"github.com/jR4dh3y/samik-bot/internal/review"
	"github.com/jR4dh3y/samik-bot/internal/runner"
	"github.com/jR4dh3y/samik-bot/internal/seal"
	"github.com/jR4dh3y/samik-bot/internal/store"
)

func TestStartRecoversInterruptedReviewWithNewDurableClaim(t *testing.T) {
	path := t.TempDir() + "/reviews.db"
	cipher, err := seal.New("engine-recovery-test")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path, cipher)
	if err != nil {
		t.Fatal(err)
	}
	review := &store.Review{
		RepoFull:          "o/r",
		RepositoryID:      1,
		PRNumber:          1,
		InstallationID:    1,
		RequesterGitHubID: 1,
		RequesterLogin:    "alice",
		TriggerCommentID:  1,
	}
	if err := st.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := st.AcquireServiceLeaseWithFence("engine-recovery-owner", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire recovery lease = %+v, %v, %v", lease, acquired, err)
	}
	if err := st.StartReview(review.ID, "model", 1, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	interrupted, err := st.Review(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Status != store.StatusRunning || interrupted.ExecutionGeneration == 0 || interrupted.StartedAt.IsZero() {
		t.Fatalf("interrupted review = %+v", interrupted)
	}
	if released, err := st.ReleaseServiceLease(lease.OwnerToken, lease.Fence); err != nil || !released {
		t.Fatalf("release interrupted test lease = %v, %v", released, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(path, cipher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	engine := NewEngine(&config.Config{}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.pollInterval = time.Hour
	if err := engine.Start(1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := engine.Stop(ctx); err != nil {
			t.Errorf("stop engine: %v", err)
		}
	})

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	var got *store.Review
	for {
		got, err = st.Review(review.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.StatusFailed {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("recovered review was not claimed and safely rejected: %+v", got)
		case <-ticker.C:
		}
	}
	if got.ExecutionGeneration != interrupted.ExecutionGeneration+1 || got.StartedAt.IsZero() {
		t.Fatalf("reclaimed review = %+v", got)
	}
	if got.Error != reviewAccessRevokedMessage {
		t.Fatalf("recovered review safety failure = %q, want %q", got.Error, reviewAccessRevokedMessage)
	}
}

func TestHeadChangeSkipsAllResultComments(t *testing.T) {
	const expectedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const movedSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var (
		prCalls  int
		comments int
	)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "installation-token",
				"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			if r.Header.Get("Accept") == "application/vnd.github.diff" {
				_, _ = w.Write([]byte("diff --git a/file.go b/file.go\n"))
				return
			}
			prCalls++
			sha := expectedSHA
			if prCalls > 1 {
				sha = movedSHA
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"head":   map[string]string{"sha": sha},
				"base": map[string]any{
					"sha":  "def",
					"repo": map[string]any{"id": 7, "full_name": "o/r"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/files":
			_ = json.NewEncoder(w).Encode([]gh.File{})
		case r.Method == http.MethodPost &&
			(r.URL.Path == "/repos/o/r/issues/7/comments" || r.URL.Path == "/repos/o/r/pulls/7/comments"):
			comments++
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	t.Setenv("GITHUB_API_BASE", github.URL)

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	app, err := gh.NewApp("1", string(pemText), "webhook-secret", 707)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := seal.New("engine-head-change-test")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/reviews.db", cipher)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddKey("key", "sk-test-1234"); err != nil {
		t.Fatal(err)
	}
	rev := &store.Review{
		RepoFull:          "o/r",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(rev); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(&config.Config{
		OpenCodeBin:     "opencode2",
		OpenCodeArgs:    []string{"--standalone"},
		DefaultModel:    "opencode/reviewer",
		AdminGitHubIDs:  []int64{42},
		InstallationIDs: []int64{1},
		RepositoryIDs:   []int64{7},
	},
		st, app, pool.New(st, time.Hour), slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.run = func(_ context.Context, options runner.Options) (string, error) {
		if options.ExpectedSHA != expectedSHA {
			t.Fatalf("runner expected SHA = %q, want %q", options.ExpectedSHA, expectedSHA)
		}
		return "ignored after head recheck", nil
	}
	lease, acquired := func() (store.ServiceLease, bool) {
		lease, acquired, err := st.AcquireServiceLeaseWithFence("head-change-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return lease, acquired
	}()
	if !acquired {
		t.Fatal("head-change test lease was not acquired")
	}
	eng.startMu.Lock()
	eng.started = true
	eng.ready = true
	eng.leaseOwner = lease.OwnerToken
	eng.leaseFence = lease.Fence
	eng.startMu.Unlock()

	eng.processOne(context.Background(), rev.ID)
	_, _ = st.ReleaseServiceLease(lease.OwnerToken, lease.Fence)
	if comments != 0 {
		t.Fatalf("posted %d result comments after head changed", comments)
	}
	got, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "pull request changed") {
		t.Fatalf("stale review = %+v", got)
	}
}

func TestPreparedHeadChangeReconcilesUncertainPublication(t *testing.T) {
	for _, test := range []struct {
		name        string
		markerFound bool
		wantStatus  string
		wantPub     string
	}{
		{name: "marker found", markerFound: true, wantStatus: store.StatusFailed, wantPub: store.PublicationPosted},
		{name: "marker absent", markerFound: false, wantStatus: store.StatusReconciliationRequired, wantPub: store.PublicationReconciliationRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			var marker string
			var postCount int
			github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/issues/7/comments" {
					comments := []map[string]any{}
					if test.markerFound {
						comments = append(comments, map[string]any{
							"id":   int64(321),
							"body": marker,
							"user": map[string]int64{"id": 707},
						})
					}
					_ = json.NewEncoder(w).Encode(comments)
					return
				}
				if r.Method == http.MethodPost {
					postCount++
				}
				http.NotFound(w, r)
			}))
			defer github.Close()
			app := newBotTestApp(t, github.URL)

			cipher, err := seal.New("prepared-head-change-" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(t.TempDir()+"/reviews.db", cipher)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			review := &store.Review{
				RepoFull:          "octo/repo",
				RepositoryID:      7,
				PRNumber:          7,
				InstallationID:    1,
				RequesterGitHubID: 42,
				RequesterLogin:    "alice",
				TriggerCommentID:  99,
			}
			if err := st.CreateReview(review); err != nil {
				t.Fatal(err)
			}
			lease := testEngineLease(t, st, "prepared-head-change-"+test.name)
			claimed, ok, err := st.ClaimReviewByID(review.ID, lease.OwnerToken, lease.Fence)
			if err != nil || !ok {
				t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
			}
			if err := st.SetHeadSHA(review.ID, claimed.ExecutionGeneration, "old-sha", lease.OwnerToken, lease.Fence); err != nil {
				t.Fatal(err)
			}
			if err := st.PrepareReviewPublication(review.ID, claimed.ExecutionGeneration, store.PublicationPlan{
				SummaryMD:     "summary",
				SummaryBodyMD: "summary body",
				CommitSHA:     "old-sha",
			}, lease.OwnerToken, lease.Fence); err != nil {
				t.Fatal(err)
			}
			publications, err := st.ListReviewPublications(review.ID)
			if err != nil || len(publications) != 1 {
				t.Fatalf("publications = %+v, %v", publications, err)
			}
			publication := publications[0]
			marker = publication.Marker
			if err := st.MarkPublicationReconciliationRequired(review.ID, claimed.ExecutionGeneration, publication.ID, lease.OwnerToken, lease.Fence); err != nil {
				t.Fatal(err)
			}
			running, err := st.Review(review.ID)
			if err != nil {
				t.Fatal(err)
			}

			engine := NewEngine(&config.Config{}, st, app, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			engine.startMu.Lock()
			engine.started = true
			engine.ready = true
			engine.leaseOwner = lease.OwnerToken
			engine.leaseFence = lease.Fence
			engine.startMu.Unlock()
			defer st.ReleaseServiceLease(lease.OwnerToken, lease.Fence)

			engine.handlePreparedHeadChanged(context.Background(), "installation-token", running, engine.log)
			got, err := st.Review(review.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != test.wantStatus {
				t.Fatalf("review status = %q, want %q; review=%+v", got.Status, test.wantStatus, got)
			}
			gotPublications, err := st.ListReviewPublications(review.ID)
			if err != nil || len(gotPublications) != 1 || gotPublications[0].Status != test.wantPub {
				t.Fatalf("publication = %+v, %v; want %q", gotPublications, err, test.wantPub)
			}
			if postCount != 0 {
				t.Fatalf("reconciliation posted %d replacement comments", postCount)
			}
		})
	}
}

func TestRunWithPoolRequiresCurrentReviewLeaseBeforeOpenCode(t *testing.T) {
	st := testStoreForEngine(t)
	review := &store.Review{
		RepoFull:          "octo/repo",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	lease := testEngineLease(t, st, "run-fence")
	claimed, ok, err := st.ClaimReviewByID(review.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	if err := st.FinishReviewFailed(review.ID, claimed.ExecutionGeneration, "test lease loss", lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	engine := NewEngine(&config.Config{}, st, nil, pool.New(st, time.Hour), slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.run = func(context.Context, runner.Options) (string, error) {
		runCalled = true
		return "unexpected", nil
	}
	engine.startMu.Lock()
	engine.started = true
	engine.ready = true
	engine.leaseOwner = lease.OwnerToken
	engine.leaseFence = lease.Fence
	engine.startMu.Unlock()
	defer st.ReleaseServiceLease(lease.OwnerToken, lease.Fence)

	_, _, err = engine.runWithPool(context.Background(), "installation-token", claimed, "model", nil, engine.log)
	if !errors.Is(err, store.ErrReviewLeaseLost) {
		t.Fatalf("runWithPool error = %v, want review lease loss", err)
	}
	if runCalled {
		t.Fatal("OpenCode runner was called after the review lease was lost")
	}
}

func TestPublishPreparedFencesGitHubWriteAfterReviewLeaseLoss(t *testing.T) {
	var st *store.Store
	var reviewID int64
	var generation int64
	var owner string
	var fence int64
	markerLookupRevoked := false
	postCount := 0
	var revokeErr error
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"head":   map[string]string{"sha": "head-sha", "ref": "feature"},
				"base": map[string]any{
					"sha":  "base-sha",
					"repo": map[string]any{"id": 7, "full_name": "octo/repo"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/octo/repo/issues/7/comments":
			if !markerLookupRevoked {
				markerLookupRevoked = true
				revokeErr = st.FinishReviewFailed(reviewID, generation, "test review lease loss", owner, fence)
				if revokeErr != nil {
					http.Error(w, revokeErr.Error(), http.StatusInternalServerError)
					return
				}
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost:
			postCount++
			http.Error(w, "unexpected GitHub write", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	app := newBotTestApp(t, github.URL)
	st = testStoreForEngine(t)
	review := &store.Review{
		RepoFull:          "octo/repo",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	reviewID = review.ID
	lease := testEngineLease(t, st, "publication-write-fence")
	owner, fence = lease.OwnerToken, lease.Fence
	claimed, ok, err := st.ClaimReviewByID(review.ID, owner, fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	generation = claimed.ExecutionGeneration
	pr := &gh.PR{Number: 7}
	pr.Head.SHA = "head-sha"
	pr.Head.Ref = "feature"
	pr.Base.SHA = "base-sha"
	pr.Base.Repo.ID = 7
	pr.Base.Repo.FullName = "octo/repo"
	if err := st.SetReviewRevision(review.ID, generation, "octo/repo", pr.Head.SHA, pr.RevisionToken(), owner, fence); err != nil {
		t.Fatal(err)
	}
	if err := st.PrepareReviewPublication(review.ID, generation, store.PublicationPlan{
		SummaryMD:     "summary",
		SummaryBodyMD: "summary body",
		CommitSHA:     "head-sha",
	}, owner, fence); err != nil {
		t.Fatal(err)
	}
	running, err := st.Review(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(&config.Config{}, st, app, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.startMu.Lock()
	engine.started = true
	engine.ready = true
	engine.leaseOwner = owner
	engine.leaseFence = fence
	engine.startMu.Unlock()
	defer st.ReleaseServiceLease(owner, fence)

	err = engine.publishPrepared(context.Background(), "installation-token", running)
	if !errors.Is(err, store.ErrReviewLeaseLost) {
		t.Fatalf("publishPrepared error = %v, want review lease loss", err)
	}
	if revokeErr != nil {
		t.Fatalf("revoke review lease = %v", revokeErr)
	}
	if postCount != 0 {
		t.Fatalf("published %d comments after review lease loss", postCount)
	}
}

func TestPublishPreparedPostsSummaryBeforeFindings(t *testing.T) {
	var calls []string
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"head":   map[string]string{"sha": "head-sha", "ref": "feature"},
				"base": map[string]any{
					"sha":  "base-sha",
					"repo": map[string]any{"id": 7, "full_name": "o/r"},
				},
			})
		case r.Method == http.MethodGet &&
			(r.URL.Path == "/repos/o/r/issues/7/comments" || r.URL.Path == "/repos/o/r/pulls/7/comments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			calls = append(calls, "summary")
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 101})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/comments":
			calls = append(calls, "inline")
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 202})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	app := newBotTestApp(t, github.URL)
	st := testStoreForEngine(t)
	review := &store.Review{
		RepoFull:          "o/r",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(review); err != nil {
		t.Fatal(err)
	}
	lease := testEngineLease(t, st, "summary-before-findings")
	claimed, ok, err := st.ClaimReviewByID(review.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	pr := &gh.PR{Number: 7}
	pr.Head.SHA = "head-sha"
	pr.Head.Ref = "feature"
	pr.Base.SHA = "base-sha"
	pr.Base.Repo.ID = 7
	pr.Base.Repo.FullName = "o/r"
	if err := st.SetReviewRevision(review.ID, claimed.ExecutionGeneration, "o/r", pr.Head.SHA, pr.RevisionToken(), lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := st.PrepareReviewPublication(review.ID, claimed.ExecutionGeneration, store.PublicationPlan{
		SummaryMD:     "The change is reviewed.",
		SummaryBodyMD: "## Review\n\n### Sequence diagram\n\n```mermaid\nsequenceDiagram\n    Author->>Bot: Review\n```",
		CommitSHA:     "head-sha",
		Findings: []store.PublicationFinding{{
			Path:          "main.go",
			Line:          1,
			Side:          "RIGHT",
			Severity:      "warning",
			FindingBodyMD: "SQL injection risk",
			CommentBodyMD: "🟠 **warning**\n\nSQL injection risk",
		}},
	}, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	running, err := st.Review(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(&config.Config{}, st, app, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.startMu.Lock()
	engine.started = true
	engine.ready = true
	engine.leaseOwner = lease.OwnerToken
	engine.leaseFence = lease.Fence
	engine.startMu.Unlock()
	defer st.ReleaseServiceLease(lease.OwnerToken, lease.Fence)

	if err := engine.publishPrepared(context.Background(), "installation-token", running); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "summary,inline" {
		t.Fatalf("comment order = %q, want summary,inline", got)
	}
	stored, err := st.Review(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.StatusDone || stored.SummaryCommentID != 101 {
		t.Fatalf("stored review = %+v", stored)
	}
	findings, err := st.ListFindings(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].PostedCommentID != 202 {
		t.Fatalf("stored findings = %+v", findings)
	}
}

func testStoreForEngine(t *testing.T) *store.Store {
	t.Helper()
	cipher, err := seal.New("engine-store-test")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/reviews.db", cipher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func testEngineLease(t *testing.T, st *store.Store, owner string) store.ServiceLease {
	t.Helper()
	lease, acquired, err := st.AcquireServiceLeaseWithFence(owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire engine lease = %+v, %v, %v", lease, acquired, err)
	}
	return lease
}

func newBotTestApp(t *testing.T, base string) *gh.App {
	t.Helper()
	t.Setenv("GITHUB_API_BASE", base)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	app, err := gh.NewApp("1", string(pemText), "webhook-secret", 707)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// reviewFailureHarness fakes GitHub for a full review attempt and records the
// trigger-comment reactions and result comments the engine issues.
type reviewFailureHarness struct {
	reactions []string
	comments  int
}

func newReviewFailureHarness(t *testing.T, runErr error, logOut *bytes.Buffer) (*store.Review, *Engine, *store.Store, *reviewFailureHarness) {
	t.Helper()
	const expectedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := &reviewFailureHarness{}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"token":      "installation-token",
				"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			if r.Header.Get("Accept") == "application/vnd.github.diff" {
				_, _ = w.Write([]byte("diff --git a/file.go b/file.go\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"head":   map[string]string{"sha": expectedSHA},
				"base": map[string]any{
					"sha":  "def",
					"repo": map[string]any{"id": 7, "full_name": "o/r"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7/files":
			_ = json.NewEncoder(w).Encode([]gh.File{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/comments/99/reactions":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.reactions = append(h.reactions, body["content"])
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 1})
		case r.Method == http.MethodPost &&
			(r.URL.Path == "/repos/o/r/issues/7/comments" || r.URL.Path == "/repos/o/r/pulls/7/comments"):
			h.comments++
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(github.Close)

	app := newBotTestApp(t, github.URL)
	st := testStoreForEngine(t)
	if _, err := st.AddKey("key", "sk-test-1234"); err != nil {
		t.Fatal(err)
	}
	rev := &store.Review{
		RepoFull:          "o/r",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(rev); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if logOut != nil {
		logger = slog.New(slog.NewTextHandler(logOut, nil))
	}
	eng := NewEngine(&config.Config{
		OpenCodeBin:     "opencode2",
		OpenCodeArgs:    []string{"--standalone"},
		DefaultModel:    "opencode/reviewer",
		AdminGitHubIDs:  []int64{42},
		InstallationIDs: []int64{1},
		RepositoryIDs:   []int64{7},
	}, st, app, pool.New(st, time.Hour), logger)
	eng.run = func(context.Context, runner.Options) (string, error) { return "", runErr }
	lease := testEngineLease(t, st, "review-failure-owner")
	eng.startMu.Lock()
	eng.started = true
	eng.ready = true
	eng.leaseOwner = lease.OwnerToken
	eng.leaseFence = lease.Fence
	eng.startMu.Unlock()
	t.Cleanup(func() { _, _ = st.ReleaseServiceLease(lease.OwnerToken, lease.Fence) })
	return rev, eng, st, h
}

func TestTerminalAgentFailureMarksReviewFailed(t *testing.T) {
	var logs bytes.Buffer
	runErr := fmt.Errorf("run reviewer: %w", &runner.AgentFailure{
		Err:        fmt.Errorf("%w: agent exited", runner.ErrExecution),
		Diagnostic: "model reviewer not found",
	})
	rev, eng, st, h := newReviewFailureHarness(t, runErr, &logs)

	eng.processOne(context.Background(), rev.ID)

	got, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusFailed {
		t.Fatalf("review status = %s, want %s (stuck-running regression)", got.Status, store.StatusFailed)
	}
	if !strings.Contains(got.Error, "opencode_execution") {
		t.Fatalf("stored error = %q, want cause opencode_execution", got.Error)
	}
	if want := []string{"eyes", "-1"}; !reflect.DeepEqual(h.reactions, want) {
		t.Fatalf("reactions = %v, want %v", h.reactions, want)
	}
	if h.comments != 0 {
		t.Fatalf("posted %d result comments on failure", h.comments)
	}
	logged := logs.String()
	if !strings.Contains(logged, "cause=opencode_execution") || !strings.Contains(logged, `detail="model reviewer not found"`) {
		t.Fatalf("terminal failure log missing cause or diagnostic: %s", logged)
	}
}

func TestTerminalPiFailureUsesPiCauseAndEngineOptions(t *testing.T) {
	var logs bytes.Buffer
	runErr := fmt.Errorf("run reviewer: %w", &runner.AgentFailure{
		Err:        fmt.Errorf("%w: pi exited", runner.ErrExecution),
		Diagnostic: "pi config dir missing",
	})
	rev, eng, st, _ := newReviewFailureHarness(t, runErr, &logs)
	eng.cfg.ReviewEngine = config.EnginePi
	eng.cfg.PiBin = "pi"
	eng.cfg.PiRuntimeDir = "/opt/pi-runtime"

	var got runner.Options
	eng.run = func(_ context.Context, opts runner.Options) (string, error) {
		got = opts
		return "", runErr
	}

	eng.processOne(context.Background(), rev.ID)

	stored, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.StatusFailed {
		t.Fatalf("review status = %s, want %s", stored.Status, store.StatusFailed)
	}
	if !strings.Contains(stored.Error, "pi_execution") {
		t.Fatalf("stored error = %q, want cause pi_execution", stored.Error)
	}
	if got.Engine != config.EnginePi || got.Bin != "pi" || got.RuntimeDir != "/opt/pi-runtime" || got.RunArgs != nil {
		t.Fatalf("runner options = %+v, want the pi engine staging without run args", got)
	}
}

func TestCanceledReviewContextStaysRecoverable(t *testing.T) {
	rev, eng, st, h := newReviewFailureHarness(t, context.Canceled, nil)

	eng.processOne(context.Background(), rev.ID)

	got, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusRunning || got.Error != "" {
		t.Fatalf("canceled review = status %s, error %q; want durable running row for restart recovery", got.Status, got.Error)
	}
	if want := []string{"eyes"}; !reflect.DeepEqual(h.reactions, want) {
		t.Fatalf("reactions = %v, want %v (no failure signal on cancellation)", h.reactions, want)
	}
}

func TestPreparePublicationStoresTheDiagramForTheDashboard(t *testing.T) {
	st := testStoreForEngine(t)
	rev := &store.Review{
		RepoFull:          "o/r",
		RepositoryID:      7,
		PRNumber:          7,
		InstallationID:    1,
		RequesterGitHubID: 42,
		RequesterLogin:    "alice",
		TriggerCommentID:  99,
	}
	if err := st.CreateReview(rev); err != nil {
		t.Fatal(err)
	}
	lease := testEngineLease(t, st, "diagram-in-summary")
	claimed, ok, err := st.ClaimReviewByID(rev.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	pr := &gh.PR{Number: 7}
	pr.Head.SHA = "head-sha"
	pr.Head.Ref = "feature"
	pr.Base.SHA = "base-sha"
	pr.Base.Repo.ID = 7
	pr.Base.Repo.FullName = "o/r"
	if err := st.SetReviewRevision(rev.ID, claimed.ExecutionGeneration, "o/r", pr.Head.SHA, pr.RevisionToken(), lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(&config.Config{BotUsername: "samik-bot"}, st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	running, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.preparePublication(running, pr, review.NewDiffIndex(nil), review.ReviewResult{
		SummaryMD:       "Narrative summary.",
		SequenceDiagram: "sequenceDiagram\n    A->>B: Call",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Review(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Narrative summary.", "```mermaid", "sequenceDiagram", "A->>B: Call"} {
		if !strings.Contains(stored.SummaryMD, want) {
			t.Fatalf("stored summary_md missing %q:\n%s", want, stored.SummaryMD)
		}
	}
}
