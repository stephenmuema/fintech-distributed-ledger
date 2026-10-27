package httpapi_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Several instances of Zirnis on one database (ADR 0030). They need a server engine
// (ZIRNIS_TEST_DATASOURCE=postgres …): one process owns a SQLite file.

// anotherInstance starts a second Zirnis on h's database.
func anotherInstance(t *testing.T, h *harness) *harness {
	t.Helper()
	if h.sys.Store.Dialect() == store.SQLite {
		t.Skip("several instances share a server database only")
	}
	return newHarness(t, func(c *config.Config) {
		c.Datasource, c.DatabaseURL = h.sys.Config.Datasource, h.sys.Config.DatabaseURL
	})
}

func TestAChangeAuthorisedOnOneInstanceReachesTheOthers(t *testing.T) {
	a := newHarness(t)
	b := anotherInstance(t, a)
	airtel := newFakeProvider(mobileMoney("Airtel Money"))
	serveModule(a, "airtel-module", provapi.Module{"airtel": airtel}, 2000)
	onboardProvider(a, "AIRTEL", "airtel", "airtel-module", 1)
	check(t, a.sys.App.Providers.Get("airtel") != nil, "the authorising instance routes to it at once")
	check(t, b.sys.App.Providers.Get("airtel") == nil, "the other has not reloaded yet")

	if err := b.sys.App.ReloadIfChanged(context.Background()); err != nil { // its worker does this every few seconds
		t.Fatal(err)
	}
	check(t, b.sys.App.Providers.Get("airtel") != nil, "the other routes to it after reloading")
}

func TestCloseOfBusinessRunsOnOneInstanceAtATime(t *testing.T) {
	a := newHarness(t)
	b := anotherInstance(t, a)
	ctx := context.Background()
	if held, err := b.sys.Store.Lease(ctx, "cob", b.sys.App.Instance, time.Minute); err != nil || !held {
		t.Fatal("b starts close of business: ", held, err)
	}

	_, err := a.sys.App.Cob.Run(ctx)
	check(t, err != nil && strings.Contains(err.Error(), "another instance"), "a may not run it meanwhile: ", err)

	if _, err := b.sys.Store.Lease(ctx, "cob", b.sys.App.Instance, -time.Second); err != nil { // b finishes
		t.Fatal(err)
	}
	run, err := a.sys.App.Cob.Run(ctx)
	check(t, err == nil && run != nil, "then a can: ", err)
}

func TestCloseOfBusinessWaitsForWorkInFlightOnEveryInstance(t *testing.T) {
	a := newHarness(t)
	b := anotherInstance(t, a)
	ctx := context.Background()
	if err := b.sys.App.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	// A unit of work on b begins while the day is still open...
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- b.sys.Store.Atomic(ctx, func(tx *store.Tx) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	stopAcks := make(chan struct{})
	defer close(stopAcks)
	go func() { // b's acknowledgement loop
		for {
			select {
			case <-stopAcks:
				return
			case <-time.After(20 * time.Millisecond):
				_ = b.sys.App.AcknowledgeCob(ctx)
			}
		}
	}()

	// ...and a closes the day meanwhile: it switches the date, then waits for b.
	cob := make(chan error, 1)
	go func() {
		_, err := a.sys.App.Cob.Run(ctx)
		cob <- err
	}()
	select {
	case err := <-cob:
		t.Fatal("close of business ran its end-of-day jobs while b could still book to the day: ", err)
	case <-time.After(time.Second):
	}

	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-cob:
		check(t, err == nil, "close of business finishes once b acknowledged: ", err)
	case <-time.After(30 * time.Second):
		t.Fatal("close of business never went on after b acknowledged")
	}
	check(t, a.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
}

func TestCloseOfBusinessDoesNotWaitForAnInstanceThatIsGone(t *testing.T) {
	a := newHarness(t)
	b := anotherInstance(t, a)
	ctx := context.Background()
	if err := b.sys.App.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	// b stopped heartbeating a minute ago (it crashed; its units of work rolled back).
	err := b.sys.Store.Atomic(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE instances SET heartbeat_at = ? WHERE id = ?", time.Now().UTC().Add(-time.Minute), b.sys.App.Instance)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.sys.App.Cob.Run(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		check(t, err == nil, "close of business runs: ", err)
	case <-time.After(30 * time.Second):
		t.Fatal("close of business waited for an instance that is gone")
	}
}
