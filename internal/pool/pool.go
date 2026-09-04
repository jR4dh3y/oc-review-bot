// Package pool hands out OpenCode Zen API keys, spreading usage across the
// pool so no single key hits its daily limit.
package pool

import (
	"errors"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

// ErrEmpty is returned when no usable key exists.
var ErrEmpty = errors.New("no usable Zen API keys in the pool")

// Pool rotates across the enabled, non-cooling-down keys.
type Pool struct {
	st *store.Store
	// CooldownOnExhausted is how long a key rests after a quota failure.
	CooldownOnExhausted time.Duration
}

func New(st *store.Store, cooldown time.Duration) *Pool {
	if cooldown <= 0 {
		cooldown = timeUntilUTCMidnight()
	}
	return &Pool{st: st, CooldownOnExhausted: cooldown}
}

// Acquire picks the key with the fewest requests today and records one use.
func (p *Pool) Acquire() (*store.ZenKey, error) {
	cands, err := p.st.PoolCandidates()
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, ErrEmpty
	}
	k := &cands[0]
	if err := p.st.RecordUsage(k.ID); err != nil {
		return nil, err
	}
	return k, nil
}

// Exhausted puts a key on cooldown after a quota/rate-limit failure.
func (p *Pool) Exhausted(keyID int64) error {
	return p.st.CoolKey(keyID, time.Now().UTC().Add(p.CooldownOnExhausted))
}

// timeUntilUTCMidnight matches the free-tier daily reset.
func timeUntilUTCMidnight() time.Duration {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return midnight.Sub(now)
}
