package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidServiceLeaseOwner = errors.New("service lease owner token is required")
	ErrInvalidServiceLeaseTTL   = errors.New("service lease TTL must be positive")
)

// Fixed-width UTC RFC3339 timestamps keep SQLite TEXT comparisons chronological.
const serviceLeaseTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// ServiceLease is the immutable fencing term assigned to one lease ownership
// epoch. The fence is never reused, including after an orderly release.
type ServiceLease struct {
	OwnerToken string
	Fence      int64
	ExpiresAt  time.Time
}

// AcquireServiceLease is retained as a small compatibility wrapper for callers
// that only need to know whether acquisition succeeded. Worker code must use
// AcquireServiceLeaseWithFence and retain the returned term.
func (s *Store) AcquireServiceLease(owner string, ttl time.Duration) (bool, error) {
	_, acquired, err := s.AcquireServiceLeaseWithFence(owner, ttl)
	return acquired, err
}

// AcquireServiceLeaseWithFence atomically takes the singleton lease if it is
// absent or expired and returns a monotonically increasing fencing term.
func (s *Store) AcquireServiceLeaseWithFence(owner string, ttl time.Duration) (ServiceLease, bool, error) {
	if err := validateServiceLease(owner, ttl); err != nil {
		return ServiceLease{}, false, err
	}

	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return ServiceLease{}, false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO service_leases (id, owner_token, fence, expires_at)
		VALUES (1, ?, 1, ?)
		ON CONFLICT(id) DO UPDATE SET
			owner_token = excluded.owner_token,
			fence = service_leases.fence + 1,
			expires_at = excluded.expires_at
		WHERE service_leases.expires_at <= ?`,
		owner, serviceLeaseTime(now.Add(ttl)), serviceLeaseTime(now))
	if err != nil {
		return ServiceLease{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ServiceLease{}, false, err
	}
	if rows != 1 {
		if err := tx.Commit(); err != nil {
			return ServiceLease{}, false, err
		}
		return ServiceLease{}, false, nil
	}

	var lease ServiceLease
	var expires string
	if err := tx.QueryRow(`SELECT owner_token, fence, expires_at FROM service_leases WHERE id = 1`).Scan(
		&lease.OwnerToken, &lease.Fence, &expires); err != nil {
		return ServiceLease{}, false, err
	}
	lease.ExpiresAt, err = time.Parse(serviceLeaseTimeLayout, expires)
	if err != nil {
		return ServiceLease{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ServiceLease{}, false, err
	}
	return lease, true, nil
}

// RenewServiceLease refreshes a live lease only when both its owner and fencing
// term match. A stale process cannot renew a later ownership epoch.
func (s *Store) RenewServiceLease(owner string, fence int64, ttl time.Duration) (bool, error) {
	if err := validateServiceLease(owner, ttl); err != nil {
		return false, err
	}
	if fence < 1 {
		return false, ErrServiceLeaseFence
	}

	now := time.Now().UTC()
	result, err := s.db.Exec(`UPDATE service_leases
		SET expires_at = ?
		WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?`,
		serviceLeaseTime(now.Add(ttl)), owner, fence, serviceLeaseTime(now))
	if err != nil {
		return false, err
	}
	return serviceLeaseMutation(result)
}

// ServiceLeaseCurrent reports whether owner currently holds the singleton. A
// fence is required for worker decisions; the optional form remains a read-only
// diagnostic for existing health/tests and never authorizes a mutation.
func (s *Store) ServiceLeaseCurrent(owner string, fences ...int64) (bool, error) {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return false, err
	}
	query := `SELECT COUNT(*) FROM service_leases WHERE id = 1 AND owner_token = ? AND expires_at > ?`
	args := []any{owner, serviceLeaseTime(time.Now().UTC())}
	if len(fences) > 0 {
		if fences[0] < 1 {
			return false, ErrServiceLeaseFence
		}
		query = `SELECT COUNT(*) FROM service_leases WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?`
		args = []any{owner, fences[0], serviceLeaseTime(time.Now().UTC())}
	}
	var count int
	err := s.db.QueryRow(query, args...).Scan(&count)
	return count == 1, err
}

// CurrentServiceLease returns the live term for owner without authorizing any
// mutation. Workers retain the term returned by acquisition; this read-only
// helper is useful for diagnostics and graceful test/setup handoff.
func (s *Store) CurrentServiceLease(owner string) (ServiceLease, bool, error) {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return ServiceLease{}, false, err
	}
	var lease ServiceLease
	var expires string
	err := s.db.QueryRow(`SELECT owner_token, fence, expires_at FROM service_leases
		WHERE id = 1 AND owner_token = ? AND expires_at > ?`, owner, serviceLeaseTime(time.Now().UTC())).Scan(
		&lease.OwnerToken, &lease.Fence, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceLease{}, false, nil
	}
	if err != nil {
		return ServiceLease{}, false, err
	}
	lease.ExpiresAt, err = time.Parse(serviceLeaseTimeLayout, expires)
	if err != nil {
		return ServiceLease{}, false, err
	}
	return lease, true, nil
}

func serviceLeaseCurrentTx(tx *sql.Tx, owner string, fence int64, at time.Time) (bool, error) {
	if err := validateServiceLeaseTerm(owner, fence); err != nil {
		return false, err
	}
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM service_leases
		WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?`,
		owner, fence, serviceLeaseTime(at)).Scan(&count)
	return count == 1, err
}

func requireServiceLeaseTx(tx *sql.Tx, owner string, fence int64, at time.Time) error {
	current, err := serviceLeaseCurrentTx(tx, owner, fence, at)
	if err != nil {
		return err
	}
	if !current {
		return ErrServiceLeaseLost
	}
	return nil
}

// ReleaseServiceLease expires the current term without deleting its row. The
// retained fence prevents release/reacquire cycles from reusing a term.
func (s *Store) ReleaseServiceLease(owner string, fence int64) (bool, error) {
	if err := validateServiceLeaseTerm(owner, fence); err != nil {
		return false, err
	}
	result, err := s.db.Exec(`UPDATE service_leases SET expires_at = ?
		WHERE id = 1 AND owner_token = ? AND fence = ? AND expires_at > ?`,
		serviceLeaseTime(time.Now().UTC()), owner, fence, serviceLeaseTime(time.Now().UTC()))
	if err != nil {
		return false, err
	}
	return serviceLeaseMutation(result)
}

func validateServiceLease(owner string, ttl time.Duration) error {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return err
	}
	if ttl <= 0 {
		return ErrInvalidServiceLeaseTTL
	}
	return nil
}

func validateServiceLeaseOwner(owner string) error {
	if strings.TrimSpace(owner) == "" {
		return ErrInvalidServiceLeaseOwner
	}
	return nil
}

func validateServiceLeaseTerm(owner string, fence int64) error {
	if err := validateServiceLeaseOwner(owner); err != nil {
		return err
	}
	if fence < 1 {
		return ErrServiceLeaseFence
	}
	return nil
}

func serviceLeaseMutation(result sql.Result) (bool, error) {
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func serviceLeaseTime(t time.Time) string {
	return t.UTC().Format(serviceLeaseTimeLayout)
}

// OutboundHandoffTimeout is the safety window for an outbound GitHub request
// whose result may have been accepted remotely before the worker crashed.
const OutboundHandoffTimeout = 45 * time.Second

// leaseDeadline reports whether a send began before the handoff window. It is
// deliberately kept in the store package so all outbox recovery uses one bound.
const outboundHandoffTimeout = OutboundHandoffTimeout

func leaseDeadline(at time.Time) string {
	return serviceLeaseTime(at.Add(-outboundHandoffTimeout))
}
