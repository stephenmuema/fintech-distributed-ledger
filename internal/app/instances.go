package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Several instances share the database (ADR 0030). Each records a heartbeat; close of business,
// after switching the business date, waits until every live instance has acknowledged the switch.
// An instance acknowledges once its own units of work that began before it saw the switch have
// finished, so none of them can still book to the day being closed.

const (
	// HeartbeatInterval is how often an instance records that it is alive.
	HeartbeatInterval = 3 * time.Second
	// instanceTimeout: an instance silent this long is gone; close of business stops waiting for
	// it (its units of work ended with it, rolled back).
	instanceTimeout = 30 * time.Second
)

// Heartbeat records that this instance is alive. Every instance runs it every HeartbeatInterval,
// whether or not it runs workers, and apart from AcknowledgeCob: a long wait there must not make
// the instance look gone.
func (a *App) Heartbeat(ctx context.Context) error {
	now := time.Now().UTC() // instances compare wall-clock times, not the business date
	return a.db.Atomic(ctx, func(tx *store.Tx) error {
		res, err := tx.Exec("UPDATE instances SET heartbeat_at = ? WHERE id = ?", now, a.Instance)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		if _, err := tx.Exec("INSERT INTO instances (id, started_at, heartbeat_at) VALUES (?, ?, ?)", a.Instance, now, now); err != nil {
			return err
		}
		// Forget instances gone for a day (restarted pods).
		_, err = tx.Exec("DELETE FROM instances WHERE heartbeat_at < ?", now.Add(-24*time.Hour))
		return err
	})
}

// AcknowledgeCob acknowledges a close of business that switched the date, once this instance's
// units of work that began before it saw the switch have finished. Every instance runs it every
// HeartbeatInterval.
func (a *App) AcknowledgeCob(ctx context.Context) error {
	seen := time.Now()
	dates, err := a.Dates.Get(a.read(ctx))
	if err != nil || dates.Status != domain.StatusCloseOfBusiness || dates.CurrentCobRunID == nil {
		return err
	}
	run := *dates.CurrentCobRunID
	if acked, err := store.Exists(a.read(ctx), "SELECT 1 FROM cob_acks WHERE run_id = ? AND instance_id = ?", run, a.Instance); err != nil || acked {
		return err
	}
	if err := a.db.WaitForWriters(ctx, seen); err != nil {
		return err
	}
	return a.ackCob(ctx, run)
}

func (a *App) ackCob(ctx context.Context, run uuid.UUID) error {
	err := a.db.Atomic(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO cob_acks (run_id, instance_id, acked_at) VALUES (?, ?, ?)", run, a.Instance, time.Now().UTC())
		return err
	})
	if store.IsUniqueViolation(err) {
		return nil // acknowledged already (a resumed run) or meanwhile; the failed statement rolled back
	}
	return err
}

// awaitInstances is close of business waiting, after switching the date, for every live
// instance to acknowledge run. Its own instance acknowledges here.
func (a *App) awaitInstances(ctx context.Context, run uuid.UUID) error {
	if err := a.db.WaitForWriters(ctx, time.Now()); err != nil {
		return err
	}
	if err := a.ackCob(ctx, run); err != nil {
		return err
	}
	logged := time.Now()
	for {
		waiting, err := store.Select[struct {
			ID string `db:"id"`
		}](a.read(ctx), "SELECT id FROM instances WHERE heartbeat_at > ? AND id NOT IN (SELECT instance_id FROM cob_acks WHERE run_id = ?)",
			time.Now().UTC().Add(-instanceTimeout), run)
		if err != nil || len(waiting) == 0 {
			return err
		}
		if time.Since(logged) > 30*time.Second {
			a.Logger.Warn("close of business waiting for instances to finish booking to the closing day", "instances", len(waiting), "first", waiting[0].ID)
			logged = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Deregister removes this instance when it stops, so close of business doesn't wait for it.
func (a *App) Deregister(ctx context.Context) error {
	return a.db.Atomic(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("DELETE FROM instances WHERE id = ?", a.Instance)
		return err
	})
}
