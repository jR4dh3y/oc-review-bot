// Package pool hands out OpenCode Zen API keys, balancing configured request
// load while cooling keys after provider quota or rate-limit failures.
package pool

import (
	"errors"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

// ErrEmpty is returned when no usable key exists.
var ErrEmpty = errors.New("no usable Zen API keys in the pool")

const defaultCooldown = time.Hour

// Pool rotates across the enabled, non-cooling-down keys.
type Pool struct {
	st *store.Store
	// CooldownOnExhausted is how long a key rests after a quota failure.
	CooldownOnExhausted time.Duration
}

func New(st *store.Store, cooldown time.Duration) *Pool {
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	return &Pool{st: st, CooldownOnExhausted: cooldown}
}

// Acquire picks the key with the fewest requests today and records one use.
func (p *Pool) Acquire() (*store.ZenKey, error) {
	k, err := p.st.AcquireKey()
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrEmpty
	}
	return k, err
}

// Exhausted puts a key on cooldown after a quota/rate-limit failure.
func (p *Pool) Exhausted(keyID int64) error {
	return p.st.CoolKey(keyID, time.Now().UTC().Add(p.CooldownOnExhausted))
}
