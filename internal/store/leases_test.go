package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/clock"
)

func TestALeaseHasOneHolderUntilItExpires(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	lease := func(holder string, ttl time.Duration) bool {
		t.Helper()
		held, err := st.Lease(ctx, "operations", holder, ttl)
		if err != nil {
			t.Fatal(err)
		}
		return held
	}
	if !lease("a", 200*time.Millisecond) || lease("b", time.Minute) {
		t.Fatal("a takes the lease and b must not")
	}
	if !lease("a", 200*time.Millisecond) {
		t.Fatal("the holder renews it")
	}
	time.Sleep(250 * time.Millisecond)
	if !lease("b", time.Minute) || lease("a", time.Minute) {
		t.Fatal("b takes over the expired lease, and a no longer has it")
	}
}

func TestWithLeaseRunsOneHolderAtATimeAndRenewsAsItRuns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"), clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var running, overlaps, ran atomic.Int32
	var wg sync.WaitGroup
	for _, holder := range []string{"a", "b", "c"} {
		wg.Go(func() {
			// The job outlasts the lease's ttl: renewals keep the others out.
			did, err := st.WithLease(t.Context(), "cob", holder, 90*time.Millisecond, func() error {
				if running.Add(1) > 1 {
					overlaps.Add(1)
				}
				time.Sleep(300 * time.Millisecond)
				running.Add(-1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			if did {
				ran.Add(1)
			}
		})
	}
	wg.Wait()
	if overlaps.Load() != 0 || ran.Load() < 1 {
		t.Fatalf("overlaps %d, ran %d", overlaps.Load(), ran.Load())
	}
	if held, _ := st.Lease(t.Context(), "cob", "d", time.Minute); !held {
		t.Fatal("a released lease is free at once")
	}
}
