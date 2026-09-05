package store

import (
	"errors"
	"testing"
	"time"
)

func TestServiceLeaseFirstAcquisition(t *testing.T) {
	s := testStore(t)

	lease, acquired, err := s.AcquireServiceLeaseWithFence("owner-a", time.Minute)
	if err != nil || !acquired || lease.Fence < 1 {
		t.Fatalf("first acquisition = %+v, %v, %v", lease, acquired, err)
	}
	current, err := s.ServiceLeaseCurrent("owner-a", lease.Fence)
	if err != nil || !current {
		t.Fatalf("first owner current = %v, %v", current, err)
	}
	if acquired, err := s.AcquireServiceLease("owner-a", time.Minute); err != nil || acquired {
		t.Fatalf("live owner re-acquisition = %v, %v", acquired, err)
	}
}

func TestServiceLeaseRejectsCompetingOwner(t *testing.T) {
	first, second := serviceLeaseStores(t)
	type attempt struct {
		owner    string
		acquired bool
		lease    ServiceLease
		err      error
	}
	start := make(chan struct{})
	results := make(chan attempt, 2)
	for owner, s := range map[string]*Store{"owner-a": first, "owner-b": second} {
		go func(owner string, s *Store) {
			<-start
			lease, acquired, err := s.AcquireServiceLeaseWithFence(owner, time.Minute)
			results <- attempt{owner: owner, acquired: acquired, lease: lease, err: err}
		}(owner, s)
	}
	close(start)

	var winner string
	var winnerLease ServiceLease
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("%s acquisition: %v", result.owner, result.err)
		}
		if result.acquired {
			if winner != "" {
				t.Fatalf("multiple owners acquired the singleton lease: %s and %s", winner, result.owner)
			}
			winner = result.owner
			winnerLease = result.lease
		}
	}
	if winner == "" {
		t.Fatal("no owner acquired the singleton lease")
	}
	loser := "owner-a"
	if winner == loser {
		loser = "owner-b"
	}
	if renewed, err := first.RenewServiceLease(loser, winnerLease.Fence, time.Minute); err != nil || renewed {
		t.Fatalf("competing renewal = %v, %v", renewed, err)
	}
	current, err := second.ServiceLeaseCurrent(winner, winnerLease.Fence)
	if err != nil || !current {
		t.Fatalf("winning owner current = %v, %v", current, err)
	}
}

func TestServiceLeaseOwnerCanRenew(t *testing.T) {
	s := testStore(t)
	lease, acquired, err := s.AcquireServiceLeaseWithFence("owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquisition = %v, %v", acquired, err)
	}
	before := serviceLeaseExpirationForTest(t, s)

	renewed, err := s.RenewServiceLease("owner-a", lease.Fence, 2*time.Minute)
	if err != nil || !renewed {
		t.Fatalf("renewal = %v, %v", renewed, err)
	}
	after := serviceLeaseExpirationForTest(t, s)
	if !after.After(before) {
		t.Fatalf("renewal expiry = %s, want after %s", after, before)
	}
}

func TestServiceLeaseExpiredOwnerCannotRenewAndCanBeReplaced(t *testing.T) {
	s := testStore(t)
	lease, acquired, err := s.AcquireServiceLeaseWithFence("owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquisition = %v, %v", acquired, err)
	}
	if _, err := s.db.Exec(`UPDATE service_leases SET expires_at = ? WHERE id = 1`,
		serviceLeaseTime(time.Now().UTC().Add(-time.Second))); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	if renewed, err := s.RenewServiceLease("owner-a", lease.Fence, time.Minute); err != nil || renewed {
		t.Fatalf("expired owner renewal = %v, %v", renewed, err)
	}
	replacement, acquired, err := s.AcquireServiceLeaseWithFence("owner-b", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("expiry takeover = %v, %v", acquired, err)
	}
	if released, err := s.ReleaseServiceLease("owner-a", lease.Fence); err != nil || released {
		t.Fatalf("stale owner release = %v, %v", released, err)
	}
	oldCurrent, err := s.ServiceLeaseCurrent("owner-a")
	if err != nil || oldCurrent {
		t.Fatalf("expired owner current = %v, %v", oldCurrent, err)
	}
	newCurrent, err := s.ServiceLeaseCurrent("owner-b", replacement.Fence)
	if err != nil || !newCurrent {
		t.Fatalf("replacement owner current = %v, %v", newCurrent, err)
	}
}

func TestServiceLeaseReleaseRequiresCurrentOwner(t *testing.T) {
	s := testStore(t)
	lease, acquired, err := s.AcquireServiceLeaseWithFence("owner-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first acquisition = %v, %v", acquired, err)
	}

	if released, err := s.ReleaseServiceLease("owner-b", lease.Fence); err != nil || released {
		t.Fatalf("competing release = %v, %v", released, err)
	}
	if released, err := s.ReleaseServiceLease("owner-a", lease.Fence); err != nil || !released {
		t.Fatalf("owner release = %v, %v", released, err)
	}
	current, err := s.ServiceLeaseCurrent("owner-a")
	if err != nil || current {
		t.Fatalf("released owner current = %v, %v", current, err)
	}
}

func TestServiceLeaseRejectsInvalidOwnerAndTTL(t *testing.T) {
	s := testStore(t)

	if _, err := s.AcquireServiceLease("\t", time.Minute); !errors.Is(err, ErrInvalidServiceLeaseOwner) {
		t.Fatalf("empty owner acquisition error = %v", err)
	}
	if _, err := s.AcquireServiceLease("owner-a", 0); !errors.Is(err, ErrInvalidServiceLeaseTTL) {
		t.Fatalf("zero TTL acquisition error = %v", err)
	}
	if _, err := s.RenewServiceLease("owner-a", 1, -time.Second); !errors.Is(err, ErrInvalidServiceLeaseTTL) {
		t.Fatalf("negative TTL renewal error = %v", err)
	}
	if _, err := s.ServiceLeaseCurrent(" "); !errors.Is(err, ErrInvalidServiceLeaseOwner) {
		t.Fatalf("empty owner check error = %v", err)
	}
	if _, err := s.ReleaseServiceLease(" ", 1); !errors.Is(err, ErrInvalidServiceLeaseOwner) {
		t.Fatalf("empty owner release error = %v", err)
	}
}

func serviceLeaseExpirationForTest(t *testing.T, s *Store) time.Time {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(`SELECT expires_at FROM service_leases WHERE id = 1`).Scan(&raw); err != nil {
		t.Fatalf("read lease expiry: %v", err)
	}
	expiresAt, err := time.Parse(serviceLeaseTimeLayout, raw)
	if err != nil {
		t.Fatalf("parse lease expiry %q: %v", raw, err)
	}
	return expiresAt
}

func serviceLeaseStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := t.TempDir() + "/leases.db"
	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open first store: %v", err)
	}
	second, err := Open(path, nil)
	if err != nil {
		first.Close()
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Errorf("close first store: %v", err)
		}
	})
	return first, second
}
