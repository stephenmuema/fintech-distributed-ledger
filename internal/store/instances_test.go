package store_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/store/storetest"
)

// These need a server engine (ZIRNIS_TEST_DATASOURCE=postgres …): several instances share one
// database there. One process owns a SQLite file.

func serverDatabase(t *testing.T) store.Options {
	t.Helper()
	d, url := storetest.FreshDatabase(t)
	if d == store.SQLite {
		t.Skip("several instances share a server database only")
	}
	dsn, err := store.DSN(d, url)
	if err != nil {
		t.Fatal(err)
	}
	return store.Options{Dialect: d, DSN: dsn}
}

func TestInstancesStartingTogetherMigrateOneAtATime(t *testing.T) {
	o := serverDatabase(t)
	var wg sync.WaitGroup
	stores := make([]*store.Store, 3)
	errs := make([]error, len(stores))
	for i := range stores {
		wg.Go(func() { stores[i], errs[i] = store.OpenWith(o, clock.System{}) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d failed to start: %v", i, err)
		}
		defer stores[i].Close()
	}
	q := stores[0].Read(t.Context())
	twice, err := store.Scalar[int](q, "SELECT COUNT(*) FROM (SELECT version FROM schema_migrations GROUP BY version HAVING COUNT(*) > 1) d")
	if err != nil || twice != 0 {
		t.Fatalf("a migration was applied twice (%d): %v", twice, err)
	}
	left, err := store.Scalar[int](q, "SELECT COUNT(*) FROM schema_migrations WHERE version LIKE 'lock:%'")
	if err != nil || left != 0 {
		t.Fatalf("the setup lock was left behind (%d): %v", left, err)
	}
}

func TestExclusiveRunsOneInstanceAtATime(t *testing.T) {
	o := serverDatabase(t)
	a, err := store.OpenWith(o, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := store.OpenWith(o, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var inside, overlaps atomic.Int32
	var wg sync.WaitGroup
	for _, st := range []*store.Store{a, b, a, b} {
		wg.Go(func() {
			err := st.Exclusive(t.Context(), func() error {
				if inside.Add(1) > 1 {
					overlaps.Add(1)
				}
				time.Sleep(50 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d holders overlapped", overlaps.Load())
	}
}
