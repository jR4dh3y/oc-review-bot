package store

import (
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

func TestUpsertUserFirstBecomesAdmin(t *testing.T) {
	s := testStore(t)

	u1, err := s.UpsertUser(1, "alice", "http://a.png", false)
	if err != nil {
		t.Fatal(err)
	}
	if !u1.IsAdmin {
		t.Fatal("first user should be admin")
	}
	u2, err := s.UpsertUser(2, "Bob", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if u2.IsAdmin {
		t.Fatal("second user should not be admin")
	}

	// Login is normalized to lowercase and upsert updates avatar.
	u1b, err := s.UpsertUser(1, "ALICE", "http://new.png", false)
	if err != nil {
		t.Fatal(err)
	}
	if u1b.Login != "alice" || u1b.AvatarURL != "http://new.png" {
		t.Fatalf("upsert mismatch: %+v", u1b)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := testStore(t)
	u, _ := s.UpsertUser(1, "alice", "", false)

	if err := s.CreateSession(u.ID, "tok123", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionUser("tok123")
	if err != nil || got.ID != u.ID {
		t.Fatalf("SessionUser = %v, %v", got, err)
	}

	s.CreateSession(u.ID, "expired", -time.Minute)
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
	s.AddKey("key-2", "sk-bbbb2222")

	// Both candidates, fewest-usage order.
	cands, err := s.PoolCandidates()
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates = %v, %v", cands, err)
	}
	if cands[0].ID != k1.ID {
		t.Fatal("expected lowest id first at equal usage")
	}

	// Usage bumps move key-1 to the back.
	s.RecordUsage(k1.ID)
	s.RecordUsage(k1.ID)
	cands, _ = s.PoolCandidates()
	if cands[0].ID == k1.ID {
		t.Fatal("key-1 should no longer be first after usage")
	}

	// Cooldown excludes the key.
	s.CoolKey(k1.ID, time.Now().UTC().Add(time.Hour))
	cands, _ = s.PoolCandidates()
	if len(cands) != 1 || cands[0].ID == k1.ID {
		t.Fatalf("cooled key should be excluded, got %d candidates", len(cands))
	}

	// Disable excludes the other.
	s.DisableKey(cands[0].ID, true)
	cands, _ = s.PoolCandidates()
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
	got, _ := s.Key(k1.ID)
	if got.Secret != "sk-aaaa1111" {
		t.Fatal("decrypt after disable mismatch")
	}
}

func TestReviewLifecycle(t *testing.T) {
	s := testStore(t)

	r := &Review{RepoFull: "o/r", PRNumber: 7, InstallationID: 42, RequesterLogin: "alice", TriggerCommentID: 99}
	if err := s.CreateReview(r); err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 {
		t.Fatal("expected id")
	}

	active, err := s.ActiveReview("o/r", 7)
	if err != nil || active.ID != r.ID {
		t.Fatalf("ActiveReview = %v, %v", active, err)
	}
	if _, err := s.ActiveReview("o/other", 7); err != ErrNotFound {
		t.Fatalf("unrelated PR should have no active review, got %v", err)
	}

	s.StartReview(r.ID, "opencode/big-pickle", 3)
	s.SetHeadSHA(r.ID, "abc123")
	s.FinishReviewDone(r.ID, "looks good", 555)

	got, err := s.Review(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.ZenKeyID != 3 || got.HeadSHA != "abc123" ||
		got.SummaryMD != "looks good" || got.SummaryCommentID != 555 {
		t.Fatalf("review mismatch: %+v", got)
	}
}

func TestNudgeDedupe(t *testing.T) {
	s := testStore(t)
	posted, err := s.NudgePosted("o/r", 1, "x")
	if err != nil || posted {
		t.Fatalf("initial nudge = %v, %v", posted, err)
	}
	s.MarkNudge("o/r", 1, "x")
	posted, err = s.NudgePosted("o/r", 1, "x")
	if err != nil || !posted {
		t.Fatalf("after mark nudge = %v, %v", posted, err)
	}
}

func TestSettings(t *testing.T) {
	s := testStore(t)
	if got := s.GetSetting("model", "fallback"); got != "fallback" {
		t.Fatalf("default = %q", got)
	}
	s.SetSetting("model", "opencode/grok-code")
	if got := s.GetSetting("model", "fallback"); got != "opencode/grok-code" {
		t.Fatalf("stored = %q", got)
	}
}
