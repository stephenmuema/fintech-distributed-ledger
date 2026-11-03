package store

import (
	"context"
	"time"
)

// Lease takes or renews the named lease for holder until ttl from now, and reports whether
// holder has it. A lease another holder let expire is taken over. Leases run on the
// wall clock: they are about time passing between processes, not the business date.
func (s *Store) Lease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	q := dbQ{s.writer, ctx, s.dialect}
	now := time.Now().UTC()
	res, err := q.Exec("UPDATE job_leases SET holder = ?, expires_at = ? WHERE name = ? AND (holder = ? OR expires_at < ?)",
		holder, now.Add(ttl), name, holder, now)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err == nil, err
	}
	if _, err := q.Exec("INSERT INTO job_leases (name, holder, expires_at) VALUES (?, ?, ?)", name, holder, now.Add(ttl)); err != nil {
		if IsUniqueViolation(err) {
			return false, nil // held by another
		}
		return false, err
	}
	return true, nil
}

// release gives a lease up early, so another instance can take it at once.
func (s *Store) release(ctx context.Context, name, holder string) {
	_, _ = dbQ{s.writer, ctx, s.dialect}.Exec("UPDATE job_leases SET expires_at = ? WHERE name = ? AND holder = ?", time.Now().UTC().Add(-time.Second), name, holder)
}

// WithLease runs fn while holder holds the named lease, renewing it every third of ttl as fn
// runs, and releases it afterwards. It reports false, without running fn, when another holder
// has the lease. A holder that dies keeps it until it expires, ttl after the last renewal.
func (s *Store) WithLease(ctx context.Context, name, holder string, ttl time.Duration, fn func() error) (bool, error) {
	if held, err := s.Lease(ctx, name, holder, ttl); err != nil || !held {
		return false, err
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(ttl / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				_, _ = s.Lease(context.WithoutCancel(ctx), name, holder, ttl)
			}
		}
	}()
	defer func() {
		close(done)
		// A renewal already in flight must finish before release. Otherwise it can
		// renew the lease after release, leaving a completed job holding the lease.
		<-stopped
		s.release(context.WithoutCancel(ctx), name, holder)
	}()
	return true, fn()
}
