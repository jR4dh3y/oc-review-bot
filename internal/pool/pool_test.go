package pool

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jR4dh3y/oc-review-bot/internal/seal"
	"github.com/jR4dh3y/oc-review-bot/internal/store"
)

func poolStore(t *testing.T) *store.Store {
	t.Helper()
	c, _ := seal.New("pool-test")
	s, err := store.Open(t.TempDir()+"/p.db", c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAcquireRotatesAcrossKeys(t *testing.T) {
	s := poolStore(t)
	s.AddKey("a", "sk-aaaa1111")
	s.AddKey("b", "sk-bbbb2222")
	s.AddKey("c", "sk-cccc3333")
	p := New(s, time.Hour)

	counts := map[int64]int{}
	for i := 0; i < 9; i++ {
		k, err := p.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		counts[k.ID]++
	}
	if len(counts) != 3 {
		t.Fatalf("used %d distinct keys, want 3", len(counts))
	}
	for id, n := range counts {
		if n != 3 {
			t.Fatalf("key %d used %d times, want 3", id, n)
		}
	}
}

func TestAcquireSkipsCooldownAndDisabled(t *testing.T) {
	s := poolStore(t)
	a, _ := s.AddKey("a", "sk-aaaa1111")
	b, _ := s.AddKey("b", "sk-bbbb2222")
	p := New(s, time.Hour)

	s.CoolKey(a.ID, time.Now().UTC().Add(time.Hour))
	s.DisableKey(b.ID, true)
	if _, err := p.Acquire(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestExhaustedCoolsDown(t *testing.T) {
	s := poolStore(t)
	a, _ := s.AddKey("a", "sk-aaaa1111")
	p := New(s, time.Hour)

	if err := p.Exhausted(a.ID); err != nil {
		t.Fatal(err)
	}
	cands, _ := s.PoolCandidates()
	if len(cands) != 0 {
		t.Fatalf("cooled key still a candidate: %+v", cands)
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("err = %v, want ErrEmpty", err)
	}
}

func TestNewUsesFixedDefaultCooldown(t *testing.T) {
	p := New(poolStore(t), 0)
	if p.CooldownOnExhausted != defaultCooldown {
		t.Fatalf("default cooldown = %s, want %s", p.CooldownOnExhausted, defaultCooldown)
	}
}

func TestAcquireAtomicallyBalancesConcurrentWorkers(t *testing.T) {
	s := poolStore(t)
	for _, label := range []string{"a", "b", "c"} {
		if _, err := s.AddKey(label, "sk-"+label+"1234"); err != nil {
			t.Fatal(err)
		}
	}
	p := New(s, time.Hour)

	const requests = 30
	start := make(chan struct{})
	keys := make(chan int64, requests)
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, err := p.Acquire()
			if err != nil {
				errs <- err
				return
			}
			keys <- key.ID
		}()
	}
	close(start)
	wg.Wait()
	close(keys)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}
	counts := map[int64]int{}
	for id := range keys {
		counts[id]++
	}
	if len(counts) != 3 {
		t.Fatalf("used %d keys, want 3", len(counts))
	}
	for id, count := range counts {
		if count != requests/3 {
			t.Fatalf("key %d used %d times, want %d", id, count, requests/3)
		}
	}
}
