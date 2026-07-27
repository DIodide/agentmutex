package mutex

import (
	"errors"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustAcquire(t *testing.T, st *Store, key, token string, o AcquireOpts) *Holder {
	t.Helper()
	res, err := st.TryAcquire(key, token, o)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Acquired {
		t.Fatalf("expected to acquire %s", key)
	}
	return res.Holder
}

func TestAcquireReleaseCycle(t *testing.T) {
	st := newTestStore(t)
	tok := NewToken()
	h := mustAcquire(t, st, "deploy:staging", tok, AcquireOpts{TTL: time.Minute, Agent: "a"})
	if h.Token != tok || h.Key != "deploy:staging" {
		t.Fatalf("bad holder: %+v", h)
	}

	// Second acquire is blocked and reports the holder.
	res, err := st.TryAcquire("deploy:staging", NewToken(), AcquireOpts{TTL: time.Minute, Agent: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Acquired || res.Holder == nil || res.Holder.Agent != "a" {
		t.Fatalf("expected block by a, got %+v", res)
	}

	// Wrong token cannot release.
	if _, err := st.Release("deploy:staging", "bogus", false); err == nil {
		t.Fatal("release with wrong token succeeded")
	} else {
		var nh *NotHolderError
		if !errors.As(err, &nh) {
			t.Fatalf("want NotHolderError, got %v", err)
		}
	}

	// Right token releases; key becomes free.
	if _, err := st.Release("deploy:staging", tok, false); err != nil {
		t.Fatal(err)
	}
	ks, err := st.Status("deploy:staging")
	if err != nil {
		t.Fatal(err)
	}
	if ks.State != "free" {
		t.Fatalf("want free, got %s", ks.State)
	}

	// Releasing again reports not held.
	if _, err := st.Release("deploy:staging", tok, false); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("want ErrNotHeld, got %v", err)
	}
}

func TestExpiryTakeover(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	st.now = func() time.Time { return now }

	tokA := NewToken()
	mustAcquire(t, st, "k", tokA, AcquireOpts{TTL: time.Second, Agent: "a"})

	// Not yet expired: B is blocked.
	res, err := st.TryAcquire("k", NewToken(), AcquireOpts{TTL: time.Minute, Agent: "b"})
	if err != nil || res.Acquired {
		t.Fatalf("expected block, got %+v, %v", res, err)
	}

	// After expiry: B displaces the dead lease.
	now = now.Add(2 * time.Second)
	tokB := NewToken()
	h := mustAcquire(t, st, "k", tokB, AcquireOpts{TTL: time.Minute, Agent: "b"})
	if h.Agent != "b" {
		t.Fatalf("takeover failed: %+v", h)
	}

	// A's stale token can no longer release or renew.
	if _, err := st.Release("k", tokA, false); err == nil {
		t.Fatal("stale token released the new lease")
	}
	if _, err := st.Renew("k", tokA, time.Minute); err == nil {
		t.Fatal("stale token renewed the new lease")
	}
}

func TestRenewExtends(t *testing.T) {
	st := newTestStore(t)
	tok := NewToken()
	h := mustAcquire(t, st, "k", tok, AcquireOpts{TTL: time.Minute, Agent: "a"})
	h2, err := st.Renew("k", tok, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !h2.ExpiresAt.After(h.ExpiresAt) {
		t.Fatalf("renew did not extend: %v -> %v", h.ExpiresAt, h2.ExpiresAt)
	}
	if h2.RenewedAt == nil {
		t.Fatal("RenewedAt not set")
	}
}

func TestQueueFIFO(t *testing.T) {
	st := newTestStore(t)
	holdTok := NewToken()
	mustAcquire(t, st, "k", holdTok, AcquireOpts{TTL: time.Minute, Agent: "holder"})

	// Two waiters join, in order.
	tok1, tok2 := NewToken(), NewToken()
	if _, err := st.Enqueue("k", Waiter{Token: tok1, Agent: "w1", EnqueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Enqueue("k", Waiter{Token: tok2, Agent: "w2", EnqueuedAt: time.Now().Add(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Release("k", holdTok, false); err != nil {
		t.Fatal(err)
	}

	// w2 polls first but must be blocked by queue head w1.
	res, err := st.TryAcquire("k", tok2, AcquireOpts{TTL: time.Minute, Agent: "w2", Enqueued: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Acquired {
		t.Fatal("w2 jumped the queue")
	}
	if res.Blocker == nil || res.Blocker.Token != tok1 {
		t.Fatalf("expected blocker w1, got %+v", res)
	}

	// w1 takes it; its queue entry is removed.
	res, err = st.TryAcquire("k", tok1, AcquireOpts{TTL: time.Minute, Agent: "w1", Enqueued: true})
	if err != nil || !res.Acquired {
		t.Fatalf("w1 should acquire: %+v, %v", res, err)
	}
	ks, err := st.Status("k")
	if err != nil {
		t.Fatal(err)
	}
	if len(ks.Waiters) != 1 || ks.Waiters[0].Token != tok2 {
		t.Fatalf("queue should hold only w2: %+v", ks.Waiters)
	}
}

func TestStaleWaiterSkipped(t *testing.T) {
	st := newTestStore(t)
	st.WaiterStaleAfter = 50 * time.Millisecond

	tokDead, tokLive := NewToken(), NewToken()
	if _, err := st.Enqueue("k", Waiter{Token: tokDead, Agent: "dead", EnqueuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond) // dead waiter stops heartbeating

	entry, err := st.Enqueue("k", Waiter{Token: tokLive, Agent: "live", EnqueuedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_ = entry

	// live is behind dead in FIFO order, but dead is stale — live may go.
	res, err := st.TryAcquire("k", tokLive, AcquireOpts{TTL: time.Minute, Agent: "live", Enqueued: true})
	if err != nil || !res.Acquired {
		t.Fatalf("live waiter blocked by stale entry: %+v, %v", res, err)
	}
}

func TestPrune(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	st.now = func() time.Time { return now }
	st.WaiterStaleAfter = 50 * time.Millisecond

	mustAcquire(t, st, "expired:key", NewToken(), AcquireOpts{TTL: time.Second, Agent: "a"})
	mustAcquire(t, st, "live:key", NewToken(), AcquireOpts{TTL: time.Hour, Agent: "b"})
	if _, err := st.Enqueue("live:key", Waiter{Token: NewToken(), Agent: "w", EnqueuedAt: now}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Second) // expire the first lease and the waiter
	res, err := st.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExpiredLeases) != 1 || res.ExpiredLeases[0] != "expired:key" {
		t.Fatalf("want [expired:key], got %+v", res.ExpiredLeases)
	}
	if res.StaleWaiters != 1 {
		t.Fatalf("want 1 stale waiter pruned, got %d", res.StaleWaiters)
	}

	// The live lease survives.
	ks, err := st.Status("live:key")
	if err != nil || ks.State != "held" {
		t.Fatalf("live lease damaged by prune: %+v, %v", ks, err)
	}
}

func TestListAndStatus(t *testing.T) {
	st := newTestStore(t)
	mustAcquire(t, st, "deploy:staging", NewToken(), AcquireOpts{TTL: time.Hour, Agent: "a", Reason: "ship v2"})
	mustAcquire(t, st, "deploy:production", NewToken(), AcquireOpts{TTL: time.Hour, Agent: "b"})

	all, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 keys, got %d", len(all))
	}
	// Sorted by key; decoded names roundtrip.
	if all[0].Key != "deploy:production" || all[1].Key != "deploy:staging" {
		t.Fatalf("bad list order/keys: %+v", all)
	}
	if all[1].Holder.Reason != "ship v2" {
		t.Fatalf("reason lost: %+v", all[1].Holder)
	}
}
