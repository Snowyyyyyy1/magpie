package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

// refreshSandbox isolates the test's magpie folders and replaces the
// bounded provider refresh with ask, so a stalled or failing ask is
// proved without Bun, a host or a network. The folders are the test's
// own, as sandbox's are, so nothing under a real HOME is touched.
func refreshSandbox(t *testing.T, ask func(ctx context.Context, epoch uint64) error) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	// a Bun is at hand, as HasBun's check needs to let the refresh start:
	// the replaced ask never runs it
	t.Setenv("MAGPIE_BUN", "bun-not-really")
	// the plugin's providers as this magpie keeps them
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	refreshWriteList(t, refreshAbsPath(t, "testdata/fake/index.js"))
	orig := refreshProviders
	if ask != nil {
		refreshProviders = ask
	}
	// the cache starts stale, as after a sign-in, and the deadlines short
	UseCached([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "FakeCo"}})
	provMu.Lock()
	provGood = false
	provMu.Unlock()
	t.Cleanup(func() {
		Settle()
		refreshProviders = orig
	})
}

func refreshAbsPath(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func refreshWriteList(t *testing.T, spec string) {
	t.Helper()
	b, err := json.Marshal(List{Plugins: []Entry{{Spec: spec}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func refreshCacheGood() bool {
	provMu.Lock()
	defer provMu.Unlock()
	return provGood
}

// A plugin host that stays alive but never answers the providers call
// must not leave the background refresh waiting for ever: it ends within
// its deadline, keeps the providers already kept, and the refusals are
// bounded rather than endless.
func TestRefreshTimeoutKeepsProviders(t *testing.T) {
	var asks atomic.Int64
	refreshSandbox(t, func(ctx context.Context, _ uint64) error {
		asks.Add(1)
		<-ctx.Done() // the host never answers
		return ctx.Err()
	})

	// the deadline is short so the test is quick (its value is what is
	// checked, not the default)
	origDeadline, origTries, origBackoff := refreshDeadline, refreshTries, refreshBackoff
	refreshDeadline, refreshTries, refreshBackoff = 150*time.Millisecond, 3, 20*time.Millisecond
	t.Cleanup(func() { refreshDeadline, refreshTries, refreshBackoff = origDeadline, origTries, origBackoff })

	ps := Cached()
	if len(ps) != 1 || ps[0].ID != "fakeco" {
		t.Fatalf("Cached answered %+v, want the providers already kept", ps)
	}

	done := make(chan struct{})
	go func() { Refreshed(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Refreshed never returned: a stalled providers call blocked the refresh")
	}

	if n := asks.Load(); n != 1 {
		t.Fatalf("the stalled providers call was asked %d times, want one bounded stall", n)
	}
	// the deadline ended each ask, rather than the call going unanswered:
	// the retries were the deadline's, not a second stall
	if refreshCacheGood() {
		t.Fatal("the failed refresh marked stale providers current")
	}
	if len(Cached()) != 1 || Cached()[0].ID != "fakeco" {
		t.Fatalf("a failed refresh lost the providers kept: %+v", Cached())
	}
}

// A background refresh that fails is retried with backoff, a bounded
// number of times; one that succeeds on a retry keeps its answer.
func TestRefreshRetriesThenSucceeds(t *testing.T) {
	var asks atomic.Int64
	var stamps []time.Time
	var mu sync.Mutex
	refreshSandbox(t, func(ctx context.Context, epoch uint64) error {
		n := asks.Add(1)
		mu.Lock()
		stamps = append(stamps, time.Now())
		mu.Unlock()
		if n == 1 {
			return errors.New("the provider's vendor is unreachable")
		}
		commitProviders([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "FakeCo", NPM: "fresh"}}, &epoch)
		return nil
	})

	origTries, origBackoff := refreshTries, refreshBackoff
	refreshTries, refreshBackoff = 3, 40*time.Millisecond
	t.Cleanup(func() { refreshTries, refreshBackoff = origTries, origBackoff })

	Cached()
	Refreshed()

	if n := asks.Load(); n != 2 {
		t.Fatalf("the failing refresh was asked %d times, want 2 (one failure, one retry)", n)
	}
	mu.Lock()
	first, second := stamps[0], stamps[1]
	mu.Unlock()
	if gap := second.Sub(first); gap < 30*time.Millisecond {
		t.Fatalf("the retry waited %v, want the backoff of at least 30ms", gap)
	}
	got := Cached()
	if len(got) != 1 || got[0].NPM != "fresh" {
		t.Fatalf("the retry's answer wasn't kept: %+v", got)
	}
}

// Two Cached calls asking at once start one refresh, not two: the check
// for a refresh already in flight and the marking of the cache are one
// step, so neither caller sees the other's half-done state.
func TestRefreshSingleFlight(t *testing.T) {
	var asks atomic.Int64
	release := make(chan struct{})
	refreshSandbox(t, func(ctx context.Context, epoch uint64) error {
		asks.Add(1)
		<-release // held until both callers have asked
		commitProviders([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "FakeCo"}}, &epoch)
		return nil
	})

	const callers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			Cached()
		}()
	}
	close(start)
	// with the refresh held, every caller has answered
	wg.Wait()
	close(release)
	Refreshed()

	if n := asks.Load(); n != 1 {
		t.Fatalf("%d concurrent Cached calls started %d refreshes, want one", callers, n)
	}
}

// Another magpie's invalidation while a refresh is running must not be
// overwritten by that refresh's older completion.
func TestRefreshOutlivedByInvalidation(t *testing.T) {
	stale := make(chan struct{})
	release := make(chan struct{})
	refreshSandbox(t, func(ctx context.Context, epoch uint64) error {
		close(stale)
		<-release
		// as the production ask does: kept only while the invalidation it
		// began under still stands
		commitProviders([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "FakeCo", NPM: "stale"}}, &epoch)
		return nil
	})

	Cached()
	<-stale
	// a sign-in saved meanwhile: the cache is invalidated again, and the
	// newer change's own refresh answers first
	forgetProviders()
	commitProviders([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "FakeCo", NPM: "newer"}}, nil)
	close(release)
	Refreshed()

	got := Cached()
	if len(got) != 1 || got[0].NPM != "newer" {
		t.Fatalf("the older completion went over the newer invalidation: %+v", got)
	}
}

func TestRefreshRetriesAfterCooldown(t *testing.T) {
	var asks atomic.Int64
	refreshSandbox(t, func(ctx context.Context, epoch uint64) error {
		if asks.Add(1) <= 3 {
			return errors.New("offline")
		}
		commitProviders([]Provider{{ID: "fakeco", Spec: refreshAbsPath(t, "testdata/fake/index.js"), Name: "Recovered"}}, &epoch)
		return nil
	})
	old := refreshBackoff
	refreshBackoff = time.Millisecond
	t.Cleanup(func() { refreshBackoff = old })
	Cached()
	Refreshed()
	Cached()
	Refreshed()
	if asks.Load() != 3 {
		t.Fatal("cooldown did not bound retries", asks.Load())
	}
	provMu.Lock()
	provRetryAt = time.Now().Add(-time.Second)
	provMu.Unlock()
	Cached()
	Refreshed()
	if asks.Load() != 4 || !refreshCacheGood() || Cached()[0].Name != "Recovered" {
		t.Fatal("stale cache did not recover", asks.Load(), Cached())
	}
}
