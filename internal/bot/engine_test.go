package bot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/config"
	"github.com/jR4dh3y/oc-review-bot/internal/gh"
	"github.com/jR4dh3y/oc-review-bot/internal/pool"
	"github.com/jR4dh3y/oc-review-bot/internal/runner"
	"github.com/jR4dh3y/oc-review-bot/internal/seal"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
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
