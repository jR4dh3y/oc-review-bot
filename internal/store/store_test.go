package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/seal"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	c, err := seal.New("test-secret")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir()+"/test.db", c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testLease(t *testing.T, s *Store, owner string) ServiceLease {
	t.Helper()
	lease, acquired, err := s.AcquireServiceLeaseWithFence(owner, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire service lease %q = %+v, %v", owner, lease, err)
	}
	return lease
}

func reviewFixture(repositoryID, prNumber, requesterID, commentID int64) *Review {
	return &Review{
		RepoFull:          "octo/repo",
		RepositoryID:      repositoryID,
		PRNumber:          prNumber,
		InstallationID:    42,
		RequesterGitHubID: requesterID,
		RequesterLogin:    "alice",
		TriggerCommentID:  commentID,
	}
}

func nudgeFixture(repositoryID, prNumber, requesterID int64, kind string) *Nudge {
	return &Nudge{
		RepoFull:          "octo/repo",
		RepositoryID:      repositoryID,
		PRNumber:          prNumber,
		InstallationID:    42,
		RequesterGitHubID: requesterID,
		RequesterLogin:    "alice",
		Kind:              kind,
	}
}

func storedNudge(t *testing.T, s *Store, id int64) *Nudge {
	t.Helper()
	n, err := scanNudge(s.db.QueryRow(`SELECT `+nudgeCols+` FROM access_nudges WHERE id = ?`, id))
	if err != nil {
		t.Fatalf("load nudge %d: %v", id, err)
	}
	return n
}

func TestUpsertUserUsesGitHubIDForIdentity(t *testing.T) {
	s := testStore(t)

	u1, err := s.UpsertUser(1, "alice", "http://a.png", false)
	if err != nil {
		t.Fatal(err)
	}
	if u1.IsAdmin {
		t.Fatal("first user must not become admin")
	}

	// forceAdmin is a current trusted allowlist decision, not a sticky flag.
	u1, err = s.UpsertUser(1, "ALICE", "http://new.png", true)
	if err != nil {
		t.Fatal(err)
	}
	if !u1.IsAdmin || u1.Login != "alice" || u1.AvatarURL != "http://new.png" {
		t.Fatal("configured allowlist user should become admin")
	}
	u1, err = s.UpsertUser(1, "alice", "http://new.png", false)
	if err != nil {
		t.Fatal(err)
	}
	if u1.IsAdmin {
		t.Fatal("removed allowlist user retained admin access")
	}

	// Login names are mutable and may be reclaimed by another GitHub account.
	u2, err := s.UpsertUser(2, "ALICE", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if u2.Login != "alice" || u2.GitHubID != 2 {
		t.Fatalf("reclaimed login user = %+v", u2)
	}
	retired, err := s.UserByGitHubID(1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(retired.Login, "retired-1-") {
		t.Fatalf("prior account retained reclaimed login: %+v", retired)
	}
	byLogin, err := s.UserByLogin("alice")
	if err != nil || byLogin.GitHubID != 2 {
		t.Fatalf("login owner = %+v, %v", byLogin, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := testStore(t)
	u, err := s.UpsertUser(1, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CreateSession(u.ID, "tok123", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionUser("tok123")
	if err != nil || got.ID != u.ID {
		t.Fatalf("SessionUser = %v, %v", got, err)
	}

	if err := s.CreateSession(u.ID, "expired", -time.Minute); err == nil {
		t.Fatal("negative session lifetime was accepted")
	}
	if err := s.CreateSession(u.ID, "expired", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), hashToken("expired")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser("expired"); err != ErrNotFound {
		t.Fatalf("expired session err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession("tok123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser("tok123"); err != ErrNotFound {
		t.Fatalf("deleted session err = %v", err)
	}
}

func TestSessionUserReportsUnreadableOrUnrevocableExpiry(t *testing.T) {
	t.Run("unreadable expiry", func(t *testing.T) {
		s := testStore(t)
		u, err := s.UpsertUser(1, "alice", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(u.ID, "malformed-expiry", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE sessions SET expires_at = 'not-a-time' WHERE token_hash = ?`, hashToken("malformed-expiry")); err != nil {
			t.Fatal(err)
		}

		if _, err := s.SessionUser("malformed-expiry"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("malformed expiry error = %v, want store failure", err)
		}
	})

	t.Run("failed expiry cleanup", func(t *testing.T) {
		s := testStore(t)
		u, err := s.UpsertUser(1, "alice", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CreateSession(u.ID, "expired-cleanup", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
			time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), hashToken("expired-cleanup")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER reject_session_deletion
			BEFORE DELETE ON sessions
			BEGIN
				SELECT RAISE(ABORT, 'session delete unavailable');
			END`); err != nil {
			t.Fatal(err)
		}

		if _, err := s.SessionUser("expired-cleanup"); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("expired cleanup error = %v, want store failure", err)
		}
	})
}

func TestDeleteAuthSessionAndOAuthStateRevokesBothCredentials(t *testing.T) {
	s := testStore(t)
	u, err := s.UpsertUser(987, "logout-user", "", false)
	if err != nil {
		t.Fatal(err)
	}
	const sessionToken = "logout-session-token"
	const oauthState = "logout-oauth-state"
	if err := s.CreateSession(u.ID, sessionToken, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthStateForClient(oauthState, "192.0.2.20", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAuthSessionAndOAuthState(sessionToken, oauthState); err != nil {
		t.Fatalf("DeleteAuthSessionAndOAuthState() error = %v", err)
	}
	if _, err := s.SessionUser(sessionToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session after logout error = %v, want %v", err, ErrNotFound)
	}
	if consumed, err := s.ConsumeOAuthState(oauthState); err != nil || consumed {
		t.Fatalf("oauth state after logout = consumed:%t err:%v", consumed, err)
	}
}

func TestOAuthStateLimitAndExpiryCleanup(t *testing.T) {
	s := testStore(t)

	for i := 0; i < maxOAuthStates; i++ {
		if err := s.CreateOAuthState(fmt.Sprintf("state-%d", i), time.Hour); err != nil {
			t.Fatalf("create state %d: %v", i, err)
		}
	}
	if err := s.CreateOAuthState("over-limit", time.Hour); !errors.Is(err, ErrOAuthStateLimit) {
		t.Fatalf("over-limit error = %v, want %v", err, ErrOAuthStateLimit)
	}

	if _, err := s.db.Exec(`UPDATE oauth_states SET expires_at = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthState("after-expiry", time.Hour); err != nil {
		t.Fatalf("expired states should be cleaned before insert: %v", err)
	}
	var active int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM oauth_states WHERE expires_at > ?`,
		time.Now().UTC().Format(time.RFC3339)).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active oauth state count = %d, want 1", active)
	}
}

func TestOAuthStateLimitsEachClientAndAllowsRevocation(t *testing.T) {
	s := testStore(t)

	for i := 0; i < maxOAuthStatesPerClient; i++ {
		if err := s.CreateOAuthStateForClient(fmt.Sprintf("client-a-%d", i), "192.0.2.1", time.Hour); err != nil {
			t.Fatalf("create client state %d: %v", i, err)
		}
	}
	if err := s.CreateOAuthStateForClient("client-a-over-limit", "192.0.2.1", time.Hour); !errors.Is(err, ErrOAuthStateClientLimit) {
		t.Fatalf("client limit error = %v, want %v", err, ErrOAuthStateClientLimit)
	}
	if err := s.CreateOAuthStateForClient("client-b", "192.0.2.2", time.Hour); err != nil {
		t.Fatalf("different client should be allowed: %v", err)
	}

	if err := s.DeleteOAuthState("client-b"); err != nil {
		t.Fatalf("DeleteOAuthState() error = %v", err)
	}
	if consumed, err := s.ConsumeOAuthState("client-b"); err != nil || consumed {
		t.Fatalf("revoked state consume = %t, %v", consumed, err)
	}
}

func TestKeyPoolRoundTrip(t *testing.T) {
	s := testStore(t)

	k1, err := s.AddKey("key-1", "sk-aaaa1111")
	if err != nil {
		t.Fatal(err)
	}
	if k1.Secret != "sk-aaaa1111" {
		t.Fatalf("decrypted secret mismatch: %q", k1.Secret)
	}
	if k1.Last4 != "1111" {
		t.Fatalf("last4 = %q", k1.Last4)
	}
	if _, err := s.AddKey("key-2", "sk-bbbb2222"); err != nil {
		t.Fatal(err)
	}

	// Both candidates, fewest-usage order.
	cands, err := s.PoolCandidates()
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	if cands[0].ID != k1.ID {
		t.Fatal("expected lowest id first at equal usage")
	}

	// Usage bumps move key-1 to the back.
	if err := s.RecordUsage(k1.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUsage(k1.ID); err != nil {
		t.Fatal(err)
	}
	cands, err = s.PoolCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].ID == k1.ID {
		t.Fatal("key-1 should no longer be first after usage")
	}

	// Cooldown excludes the key.
	if err := s.CoolKey(k1.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cands, err = s.PoolCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID == k1.ID {
		t.Fatalf("cooled key should be excluded, got %d candidates", len(cands))
	}

	// Disable excludes the other.
	if err := s.DisableKey(cands[0].ID, true); err != nil {
		t.Fatal(err)
	}
	cands, err = s.PoolCandidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("expected no candidates, got %d", len(cands))
	}

	// List is masked and decrypt still works.
	keys, err := s.ListKeys()
	if err != nil || len(keys) != 2 {
		t.Fatalf("list = %v, %v", keys, err)
	}
	if keys[0].Label == "" || keys[0].Last4 == "" {
		t.Fatal("masked key missing label/last4")
	}
	got, err := s.Key(k1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Secret != "sk-aaaa1111" {
		t.Fatal("decrypt after disable mismatch")
	}
}

func TestReviewLifecyclePersistsDurablePublication(t *testing.T) {
	s := testStore(t)

	r := reviewFixture(101, 7, 201, 99)
	r.RepoFull = "octo/renamed-repo"
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	if r.ID < 1 || r.PublicationToken == "" {
		t.Fatalf("created review = %+v", r)
	}

	active, err := s.ActiveReview(r.RepositoryID, r.PRNumber)
	if err != nil || active.ID != r.ID {
		t.Fatalf("ActiveReview = %v, %v", active, err)
	}
	if _, err := s.ActiveReview(r.RepositoryID+1, r.PRNumber); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated PR should have no active review, got %v", err)
	}

	const owner = "review-lifecycle-owner"
	lease := testLease(t, s, owner)
	claimed, ok, err := s.ClaimReviewByID(r.ID, owner, lease.Fence)
	if err != nil || !ok || claimed.Status != StatusRunning || claimed.ExecutionGeneration != 1 {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	if err := s.SetReviewExecution(r.ID, claimed.ExecutionGeneration, "opencode/big-pickle", 3, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHeadSHA(r.ID, claimed.ExecutionGeneration, "abc123", owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareReviewPublication(r.ID, claimed.ExecutionGeneration, PublicationPlan{
		SummaryMD:     "looks good",
		SummaryBodyMD: "## Review\n\nLooks good.",
		CommitSHA:     "abc123",
		Findings: []PublicationFinding{{
			Path:          "main.go",
			Line:          12,
			Side:          "RIGHT",
			Severity:      "warning",
			FindingBodyMD: "Handle this case.",
			CommentBodyMD: "Please handle this case.",
		}},
	}, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	publications, err := s.ListReviewPublications(r.ID)
	if err != nil || len(publications) != 2 {
		t.Fatalf("publications = %+v, %v", publications, err)
	}
	summary, inline := publications[0], publications[1]
	if summary.Kind != PublicationSummary || summary.Status != PublicationPending || summary.FindingID != 0 ||
		inline.Kind != PublicationInline || inline.Status != PublicationPending || inline.FindingID < 1 {
		t.Fatalf("unexpected publications: %+v", publications)
	}
	if !strings.Contains(inline.BodyMD, inline.Marker) || !strings.Contains(summary.BodyMD, summary.Marker) {
		t.Fatalf("publication markers missing: %+v", publications)
	}
	if err := s.FinishReviewDone(r.ID, claimed.ExecutionGeneration, owner, lease.Fence); !errors.Is(err, ErrReviewNotReady) {
		t.Fatalf("unfinished publication completed: %v", err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, summary.ID, owner, lease.Fence); err != nil || !ok {
		t.Fatalf("claim summary publication = %v, %v", ok, err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, summary.ID, owner, lease.Fence); err != nil || ok {
		t.Fatalf("duplicate summary publication claim during handoff = %v, %v", ok, err)
	}
	if err := s.MarkPublicationPosted(r.ID, claimed.ExecutionGeneration, summary.ID, 555, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, inline.ID, owner, lease.Fence); err != nil || !ok {
		t.Fatalf("claim inline publication = %v, %v", ok, err)
	}
	if err := s.MarkPublicationPosted(r.ID, claimed.ExecutionGeneration, inline.ID, 444, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPublicationPosted(r.ID, claimed.ExecutionGeneration, summary.ID, 999, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReviewDone(r.ID, claimed.ExecutionGeneration, owner, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if current, err := s.ReviewCompletionLeaseCurrent(r.ID, claimed.ExecutionGeneration, owner, lease.Fence); err != nil || !current {
		t.Fatalf("completion lease = %v, %v", current, err)
	}

	findings, err := s.ListFindings(r.ID)
	if err != nil || len(findings) != 1 || findings[0].PostedCommentID != 444 {
		t.Fatalf("findings = %+v, %v", findings, err)
	}
	got, err := s.Review(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.ZenKeyID != 3 || got.HeadSHA != "abc123" ||
		got.SummaryMD != "looks good" || got.SummaryCommentID != 555 ||
		got.ExecutionGeneration != claimed.ExecutionGeneration || !got.PublicationPrepared {
		t.Fatalf("review mismatch: %+v", got)
	}
}

func TestLegacyRegistrationNudgeSuppressesDuplicateAfterUpgrade(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`INSERT INTO register_nudges (repo_full, pr_number, login) VALUES (?, ?, ?)`,
		"Octo/Repo", 15, "Alice"); err != nil {
		t.Fatal(err)
	}
	n := nudgeFixture(107, 15, 207, NudgeRegistration)
	result, err := s.CreateNudgeOnce(n, "legacy-nudge-delivery")
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.DuplicateDelivery || n.ID != 0 {
		t.Fatalf("legacy nudge was recreated: %+v, nudge=%+v", result, n)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM access_nudges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy nudge created %d replacement records", count)
	}
}

func TestPublicationRecoveryWaitsForAndThenReleasesHandoff(t *testing.T) {
	s := testStore(t)
	r := reviewFixture(108, 16, 208, 109)
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	leaseA := testLease(t, s, "publication-recovery-a")
	claimed, ok, err := s.ClaimReviewByID(r.ID, leaseA.OwnerToken, leaseA.Fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	if err := s.SetHeadSHA(r.ID, claimed.ExecutionGeneration, "abc", leaseA.OwnerToken, leaseA.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareReviewPublication(r.ID, claimed.ExecutionGeneration, PublicationPlan{
		SummaryMD:     "summary",
		SummaryBodyMD: "summary body",
		CommitSHA:     "abc",
	}, leaseA.OwnerToken, leaseA.Fence); err != nil {
		t.Fatal(err)
	}
	publications, err := s.ListReviewPublications(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, publications[0].ID, leaseA.OwnerToken, leaseA.Fence); err != nil || !ok {
		t.Fatalf("claim publication = %v, %v", ok, err)
	}
	if err := s.BeginPublicationSend(r.ID, claimed.ExecutionGeneration, publications[0].ID, leaseA.OwnerToken, leaseA.Fence); err != nil {
		t.Fatalf("begin publication send = %v", err)
	}
	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	leaseB := testLease(t, s, "publication-recovery-b")
	if recovered, err := s.RecoverInterruptedReviews(leaseB.OwnerToken, leaseB.Fence); err != nil || recovered != 0 {
		t.Fatalf("fresh publication recovery = %d, %v", recovered, err)
	}
	if _, err := s.db.Exec(`UPDATE review_publications SET send_started_at = ? WHERE review_id = ?`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Minute)), r.ID); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverInterruptedReviews(leaseB.OwnerToken, leaseB.Fence); err != nil || recovered != 1 {
		t.Fatalf("expired publication recovery = %d, %v", recovered, err)
	}
	got, err := s.Review(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusQueued || got.ServiceLeaseOwner != "" {
		t.Fatalf("recovered review = %+v", got)
	}
}

func TestReviewAndNudgeRequireImmutableIDs(t *testing.T) {
	s := testStore(t)

	review := reviewFixture(101, 8, 201, 100)
	review.RepositoryID = 0
	if _, err := s.CreateReviewOnce(review, "invalid-review"); err == nil {
		t.Fatal("review without immutable repository ID was accepted")
	}
	nudge := nudgeFixture(101, 8, 201, NudgeRegistration)
	nudge.RequesterGitHubID = 0
	if _, err := s.CreateNudgeOnce(nudge, "invalid-nudge"); err == nil {
		t.Fatal("nudge without immutable requester ID was accepted")
	}
}

func TestExpiredPublicationBecomesMarkerOnlyReconciliation(t *testing.T) {
	s := testStore(t)
	r := reviewFixture(109, 17, 209, 110)
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	lease := testLease(t, s, "publication-ambiguous-owner")
	claimed, ok, err := s.ClaimReviewByID(r.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim review = %+v, %v, %v", claimed, ok, err)
	}
	if err := s.SetHeadSHA(r.ID, claimed.ExecutionGeneration, "sha", lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareReviewPublication(r.ID, claimed.ExecutionGeneration, PublicationPlan{
		SummaryMD:     "summary",
		SummaryBodyMD: "summary body",
		CommitSHA:     "sha",
	}, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	publications, err := s.ListReviewPublications(r.ID)
	if err != nil || len(publications) != 1 {
		t.Fatalf("publications = %+v, %v", publications, err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, publications[0].ID, lease.OwnerToken, lease.Fence); err != nil || !ok {
		t.Fatalf("initial publication claim = %v, %v", ok, err)
	}
	if err := s.BeginPublicationSend(r.ID, claimed.ExecutionGeneration, publications[0].ID, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatalf("begin publication send = %v", err)
	}
	if _, err := s.db.Exec(`UPDATE review_publications SET send_started_at = ? WHERE id = ?`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Minute)), publications[0].ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimPublicationForSend(r.ID, claimed.ExecutionGeneration, publications[0].ID, lease.OwnerToken, lease.Fence); !errors.Is(err, ErrPublicationReconcile) || ok {
		t.Fatalf("expired publication claim = %v, %v; want reconciliation", ok, err)
	}
	got, err := s.ListReviewPublications(r.ID)
	if err != nil || len(got) != 1 || got[0].Status != PublicationReconciliationRequired {
		t.Fatalf("publication after expiry = %+v, %v", got, err)
	}
	if err := s.MarkPublicationReconciliationRequired(r.ID, claimed.ExecutionGeneration, publications[0].ID, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishReviewReconciliationRequired(r.ID, claimed.ExecutionGeneration, "operator review", lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	review, err := s.Review(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != StatusReconciliationRequired || review.Error != "operator review" || review.ServiceLeaseOwner != "" {
		t.Fatalf("reconciliation review = %+v", review)
	}
	replacement := reviewFixture(r.RepositoryID, r.PRNumber, r.RequesterGitHubID, 111)
	if err := s.CreateReview(replacement); !errors.Is(err, ErrActiveReview) {
		t.Fatalf("replacement review admission = %v, want ErrActiveReview", err)
	}
}

func TestPurgeDeliveriesBeforeRetainsRecentMarkers(t *testing.T) {
	s := testStore(t)
	nowTime := time.Now().UTC()
	oldAt := nowTime.Add(-2 * time.Hour).Format(time.RFC3339)
	recentAt := nowTime.Add(-30 * time.Minute).Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO webhook_deliveries (delivery_id, created_at) VALUES (?, ?), (?, ?)`,
		"old-delivery", oldAt, "recent-delivery", recentAt); err != nil {
		t.Fatal(err)
	}

	purged, err := s.PurgeDeliveriesBefore(nowTime.Add(-time.Hour))
	if err != nil || purged != 1 {
		t.Fatalf("purged = %d, %v; want one old marker", purged, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE delivery_id = ?`, "old-delivery").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("old delivery marker was retained")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM webhook_deliveries WHERE delivery_id = ?`, "recent-delivery").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("recent delivery marker was purged")
	}
	var indexName string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`,
		"idx_webhook_deliveries_created_at").Scan(&indexName); err != nil {
		t.Fatalf("delivery retention index = %v", err)
	}
}

func TestAmbiguousNudgeCannotBeFinishedAsFailedOrReposted(t *testing.T) {
	s := testStore(t)
	n := nudgeFixture(110, 18, 210, NudgeEntitlement)
	if result, err := s.CreateNudgeOnce(n, "ambiguous-nudge"); err != nil || !result.Created {
		t.Fatalf("create nudge = %+v, %v", result, err)
	}
	lease := testLease(t, s, "nudge-ambiguous-owner")
	claimed, ok, err := s.ClaimNudgeByID(n.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim nudge = %+v, %v, %v", claimed, ok, err)
	}
	if err := s.MarkNudgeSending(n.ID, claimed.ExecutionGeneration, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNudgeReconciliationRequired(n.ID, claimed.ExecutionGeneration, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	stored := storedNudge(t, s, n.ID)
	if stored.Status != NudgeReconciliationRequired || stored.SendingStartedAt.IsZero() {
		t.Fatalf("ambiguous nudge = %+v", stored)
	}
	if err := s.FinishNudgeFailed(n.ID, claimed.ExecutionGeneration, lease.OwnerToken, lease.Fence); !errors.Is(err, ErrNudgeLeaseLost) {
		t.Fatalf("ambiguous nudge failure = %v, want lease/state rejection", err)
	}
	if _, err := s.db.Exec(`UPDATE access_nudges SET next_attempt_at = '', send_started_at = ? WHERE id = ?`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Minute)), n.ID); err != nil {
		t.Fatal(err)
	}
	recovered, ok, err := s.ClaimNudgeByID(n.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok || !recovered.ReconciliationRequired {
		t.Fatalf("reclaimed ambiguous nudge = %+v, %v, %v", recovered, ok, err)
	}
	if err := s.MarkNudgePosted(n.ID, recovered.ExecutionGeneration, 901, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	posted := storedNudge(t, s, n.ID)
	if posted.Status != NudgePosted || posted.PostedCommentID != 901 {
		t.Fatalf("reconciled nudge = %+v", posted)
	}
}

func TestCreateReviewOnceAtomicallyDeduplicatesActivePR(t *testing.T) {
	s := testStore(t)
	const workers = 16
	const repositoryID int64 = 102
	const requesterID int64 = 202
	start := make(chan struct{})
	results := make(chan ReviewCreateResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := s.CreateReviewOnce(
				reviewFixture(repositoryID, 9, requesterID, int64(i+1)),
				fmt.Sprintf("delivery-%d", i),
			)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	created := 0
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d active reviews, want 1", created)
	}
	reviews, err := s.ListReviews(10)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
	active, err := s.ActiveReview(repositoryID, 9)
	if err != nil || active.ID != reviews[0].ID || active.RepositoryID != repositoryID || active.RequesterGitHubID != requesterID {
		t.Fatalf("active review = %+v, %v", active, err)
	}
}

func TestClaimQueuedReviewAtomicallyAssignsOneGeneration(t *testing.T) {
	s := testStore(t)
	r := reviewFixture(103, 10, 203, 101)
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	const owner = "atomic-claim-owner"
	lease := testLease(t, s, owner)

	const workers = 16
	type claimResult struct {
		review  *Review
		claimed bool
	}
	start := make(chan struct{})
	results := make(chan claimResult, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed, ok, err := s.ClaimReviewByID(r.ID, owner, lease.Fence)
			if err != nil {
				errs <- err
				return
			}
			results <- claimResult{review: claimed, claimed: ok}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	claims := 0
	var winner *Review
	for result := range results {
		if result.claimed {
			claims++
			winner = result.review
		}
	}
	if claims != 1 || winner == nil || winner.ExecutionGeneration != 1 || winner.Status != StatusRunning {
		t.Fatalf("claims = %d, winner = %+v", claims, winner)
	}
	current, err := s.ReviewLeaseCurrent(r.ID, winner.ExecutionGeneration, owner, lease.Fence)
	if err != nil || !current {
		t.Fatalf("review lease = %v, %v", current, err)
	}
	if _, ok, err := s.ClaimReviewByID(r.ID, owner, lease.Fence); err != nil || ok {
		t.Fatalf("second claim = ok:%v err:%v", ok, err)
	}
}

func TestNextQueueClaimsRequireTheCurrentServiceLease(t *testing.T) {
	s := testStore(t)
	r := reviewFixture(108, 20, 208, 301)
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextQueuedReview("", 0); !errors.Is(err, ErrServiceLeaseRequired) {
		t.Fatalf("claim without owner = %v, want ErrServiceLeaseRequired", err)
	}
	leaseA, acquired, err := s.AcquireServiceLeaseWithFence("owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquire owner-a = %+v, %v, %v", leaseA, acquired, err)
	}
	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	leaseB, acquired, err := s.AcquireServiceLeaseWithFence("owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("takeover by owner-b = %+v, %v, %v", leaseB, acquired, err)
	}
	if _, err := s.ClaimNextQueuedReview("owner-a", leaseA.Fence); !errors.Is(err, ErrServiceLeaseLost) {
		t.Fatalf("stale review owner claim = %v, want ErrServiceLeaseLost", err)
	}
	claimed, err := s.ClaimNextQueuedReview("owner-b", leaseB.Fence)
	if err != nil || claimed.ID != r.ID || claimed.ExecutionGeneration != 1 {
		t.Fatalf("current review owner claim = %+v, %v", claimed, err)
	}

	n := nudgeFixture(108, 20, 208, NudgeRegistration)
	if result, err := s.CreateNudgeOnce(n, "lease-nudge-delivery"); err != nil || !result.Created {
		t.Fatalf("create nudge = %+v, %v", result, err)
	}
	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	leaseC, acquired, err := s.AcquireServiceLeaseWithFence("owner-c", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("takeover by owner-c = %+v, %v, %v", leaseC, acquired, err)
	}
	if _, err := s.ClaimNextQueuedNudge("owner-b", leaseB.Fence); !errors.Is(err, ErrServiceLeaseLost) {
		t.Fatalf("stale nudge owner claim = %v, want ErrServiceLeaseLost", err)
	}
	if claimed, err := s.ClaimNextQueuedNudge("owner-c", leaseC.Fence); err != nil || claimed.ID != n.ID {
		t.Fatalf("current nudge owner claim = %+v, %v", claimed, err)
	}
}

func TestCreateReviewOnceDeduplicatesWebhookDeliveryAndTrigger(t *testing.T) {
	s := testStore(t)
	first := reviewFixture(104, 11, 204, 102)
	result, err := s.CreateReviewOnce(first, "delivery-1")
	if err != nil || !result.Created {
		t.Fatalf("first create = %+v, %v", result, err)
	}

	second := reviewFixture(104, 11, 204, 102)
	result, err = s.CreateReviewOnce(second, "delivery-1")
	if err != nil || !result.DuplicateDelivery || result.Created {
		t.Fatalf("duplicate create = %+v, %v", result, err)
	}
	third := reviewFixture(104, 11, 204, 102)
	result, err = s.CreateReviewOnce(third, "delivery-2")
	if err != nil || result.Created || result.DuplicateDelivery {
		t.Fatalf("duplicate trigger = %+v, %v", result, err)
	}
	reviews, err := s.ListReviews(10)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("reviews = %+v, %v", reviews, err)
	}
}

func TestRecoverInterruptedReviewsFencesStaleGeneration(t *testing.T) {
	s := testStore(t)
	running := reviewFixture(105, 12, 205, 103)
	queued := reviewFixture(105, 13, 205, 104)
	if err := s.CreateReview(running); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateReview(queued); err != nil {
		t.Fatal(err)
	}
	const ownerA = "recovery-owner-a"
	leaseA := testLease(t, s, ownerA)
	first, ok, err := s.ClaimReviewByID(running.ID, ownerA, leaseA.Fence)
	if err != nil || !ok || first.ExecutionGeneration != 1 {
		t.Fatalf("first claim = %+v, %v, %v", first, ok, err)
	}

	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	const ownerB = "recovery-owner-b"
	leaseB, acquired, err := s.AcquireServiceLeaseWithFence(ownerB, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("takeover = %+v, %v, %v", leaseB, acquired, err)
	}
	recovered, err := s.RecoverInterruptedReviews(ownerB, leaseB.Fence)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered %d running reviews, want 1", recovered)
	}
	restored, err := s.Review(running.ID)
	if err != nil || restored.Status != StatusQueued || restored.ExecutionGeneration != first.ExecutionGeneration || !restored.StartedAt.IsZero() {
		t.Fatalf("restored review = %+v, %v", restored, err)
	}
	stillQueued, err := s.Review(queued.ID)
	if err != nil || stillQueued.Status != StatusQueued || stillQueued.ExecutionGeneration != 0 {
		t.Fatalf("queued review changed during recovery = %+v, %v", stillQueued, err)
	}
	second, ok, err := s.ClaimReviewByID(running.ID, ownerB, leaseB.Fence)
	if err != nil || !ok || second.ExecutionGeneration != first.ExecutionGeneration+1 {
		t.Fatalf("reclaimed review = %+v, %v, %v", second, ok, err)
	}
	if err := s.SetHeadSHA(running.ID, first.ExecutionGeneration, "stale", ownerA, leaseA.Fence); !errors.Is(err, ErrReviewLeaseLost) && !errors.Is(err, ErrServiceLeaseLost) {
		t.Fatalf("stale worker mutation = %v, want ErrReviewLeaseLost", err)
	}
	if err := s.SetHeadSHA(running.ID, second.ExecutionGeneration, "current", ownerB, leaseB.Fence); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationResolvesImmutableActiveReviewDuplicates(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	cipher, err := seal.New("migration-test")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, cipher)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP INDEX idx_reviews_one_active_per_pr`); err != nil {
		t.Fatal(err)
	}
	first, err := s.db.Exec(`INSERT INTO reviews
		(repo_full, repository_id, pr_number, installation_id, requester_github_id, requester_login,
		 trigger_comment_id, status, created_at)
		VALUES ('o/r', 106, 14, 1, 206, 'alice', 1, ?, ?)`, StatusQueued, now())
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := first.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.db.Exec(`INSERT INTO reviews
		(repo_full, repository_id, pr_number, installation_id, requester_github_id, requester_login,
		 trigger_comment_id, status, created_at)
		VALUES ('o/r', 106, 14, 1, 206, 'alice', 2, ?, ?)`, StatusRunning, now())
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := second.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path, cipher)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	active, err := s.ActiveReview(106, 14)
	if err != nil || active.ID != secondID || active.RepositoryID != 106 || active.RequesterGitHubID != 206 {
		t.Fatalf("active review = %+v, %v", active, err)
	}
	older, err := s.Review(firstID)
	if err != nil {
		t.Fatal(err)
	}
	if older.Status != StatusFailed || older.Error == "" {
		t.Fatalf("older review = %+v", older)
	}
}

func TestNudgeLifecycleIsDurableAndGenerationFenced(t *testing.T) {
	s := testStore(t)
	registered := nudgeFixture(107, 15, 207, NudgeRegistration)
	result, err := s.CreateNudgeOnce(registered, "nudge-delivery-1")
	if err != nil || !result.Created || registered.ID < 1 || registered.PublicationToken == "" {
		t.Fatalf("created registration nudge = %+v, %+v, %v", registered, result, err)
	}
	replay := nudgeFixture(107, 15, 207, NudgeRegistration)
	result, err = s.CreateNudgeOnce(replay, "nudge-delivery-1")
	if err != nil || !result.DuplicateDelivery || result.Created {
		t.Fatalf("duplicate nudge delivery = %+v, %v", result, err)
	}
	duplicate := nudgeFixture(107, 15, 207, NudgeRegistration)
	result, err = s.CreateNudgeOnce(duplicate, "nudge-delivery-2")
	if err != nil || result.Created || result.DuplicateDelivery {
		t.Fatalf("duplicate nudge event = %+v, %v", result, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM access_nudges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("registration nudge count = %d, %v", count, err)
	}

	const ownerA = "nudge-owner-a"
	leaseA := testLease(t, s, ownerA)
	claimed, ok, err := s.ClaimNudgeByID(registered.ID, ownerA, leaseA.Fence)
	if err != nil || !ok || claimed.Status != NudgePosting || claimed.ExecutionGeneration != 1 {
		t.Fatalf("claim registration nudge = %+v, %v, %v", claimed, ok, err)
	}
	current, err := s.NudgeLeaseCurrent(registered.ID, claimed.ExecutionGeneration, ownerA, leaseA.Fence)
	if err != nil || !current {
		t.Fatalf("registration nudge lease = %v, %v", current, err)
	}
	if marker := NudgeMarker(registered.PublicationToken); !strings.Contains(marker, registered.PublicationToken) {
		t.Fatalf("nudge marker does not contain token: %q", marker)
	}
	if err := s.MarkNudgePosted(registered.ID, claimed.ExecutionGeneration, 701, ownerA, leaseA.Fence); err != nil {
		t.Fatal(err)
	}
	posted := storedNudge(t, s, registered.ID)
	if posted.Status != NudgePosted || posted.PostedCommentID != 701 || posted.PostedAt.IsZero() {
		t.Fatalf("posted registration nudge = %+v", posted)
	}

	// The other notification kind gets its own durable outbox record.
	entitlement := nudgeFixture(107, 15, 207, NudgeEntitlement)
	result, err = s.CreateNudgeOnce(entitlement, "nudge-delivery-3")
	if err != nil || !result.Created {
		t.Fatalf("created entitlement nudge = %+v, %v", result, err)
	}
	first, ok, err := s.ClaimNudgeByID(entitlement.ID, ownerA, leaseA.Fence)
	if err != nil || !ok || first.ExecutionGeneration != 1 {
		t.Fatalf("first entitlement claim = %+v, %v, %v", first, ok, err)
	}
	if _, err := s.db.Exec(`UPDATE access_nudges SET send_started_at = ? WHERE id = ?`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Minute)), entitlement.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	const ownerB = "nudge-owner-b"
	leaseB, acquired, err := s.AcquireServiceLeaseWithFence(ownerB, time.Minute)
	if err != nil || !acquired {
		t.Fatalf("nudge takeover = %+v, %v, %v", leaseB, acquired, err)
	}
	recovered, err := s.RecoverInterruptedNudges(ownerB, leaseB.Fence)
	if err != nil || recovered != 1 {
		t.Fatalf("recovered nudges = %d, %v", recovered, err)
	}
	if _, err := s.db.Exec(`UPDATE access_nudges SET next_attempt_at = '' WHERE id = ?`, entitlement.ID); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.ClaimNudgeByID(entitlement.ID, ownerB, leaseB.Fence)
	if err != nil || !ok || second.ExecutionGeneration != first.ExecutionGeneration+1 {
		t.Fatalf("reclaimed entitlement nudge = %+v, %v, %v", second, ok, err)
	}
	if err := s.MarkNudgePosted(entitlement.ID, first.ExecutionGeneration, 702, ownerA, leaseA.Fence); !errors.Is(err, ErrNudgeLeaseLost) && !errors.Is(err, ErrServiceLeaseLost) {
		t.Fatalf("stale nudge worker mutation = %v, want ErrNudgeLeaseLost", err)
	}
	if err := s.MarkNudgePosted(entitlement.ID, second.ExecutionGeneration, 703, ownerB, leaseB.Fence); err != nil {
		t.Fatal(err)
	}
	posted = storedNudge(t, s, entitlement.ID)
	if posted.Status != NudgePosted || posted.PostedCommentID != 703 {
		t.Fatalf("posted entitlement nudge = %+v", posted)
	}
}

func TestNudgeRetryWaitsForAmbiguousSendHandoff(t *testing.T) {
	s := testStore(t)
	n := nudgeFixture(109, 17, 209, NudgeEntitlement)
	result, err := s.CreateNudgeOnce(n, "ambiguous-nudge")
	if err != nil || !result.Created {
		t.Fatalf("create nudge = %+v, %v", result, err)
	}
	lease := testLease(t, s, "ambiguous-nudge-owner")
	claimed, ok, err := s.ClaimNudgeByID(n.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok {
		t.Fatalf("claim nudge = %+v, %v, %v", claimed, ok, err)
	}
	if err := s.MarkNudgeSending(n.ID, claimed.ExecutionGeneration, lease.OwnerToken, lease.Fence); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueNudge(n.ID, claimed.ExecutionGeneration, time.Second, lease.OwnerToken, lease.Fence); !errors.Is(err, ErrNudgeReconcile) {
		t.Fatalf("ambiguous nudge requeue = %v, want reconciliation", err)
	}
	if _, ok, err := s.ClaimNudgeByID(n.ID, lease.OwnerToken, lease.Fence); err != nil || ok {
		t.Fatalf("nudge claimed before handoff elapsed = %v, %v", ok, err)
	}
	if _, err := s.db.Exec(`UPDATE access_nudges SET next_attempt_at = '', send_started_at = ? WHERE id = ?`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Minute)), n.ID); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.ClaimNudgeByID(n.ID, lease.OwnerToken, lease.Fence)
	if err != nil || !ok || second.ExecutionGeneration != claimed.ExecutionGeneration+1 {
		t.Fatalf("nudge claimed after handoff = %+v, %v, %v", second, ok, err)
	}
}

func TestSettings(t *testing.T) {
	s := testStore(t)
	if got := s.GetSetting("model", "fallback"); got != "fallback" {
		t.Fatalf("default = %q", got)
	}
	if err := s.SetSetting("model", "opencode/grok-code"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetSetting("model", "fallback"); got != "opencode/grok-code" {
		t.Fatalf("stored = %q", got)
	}
}
