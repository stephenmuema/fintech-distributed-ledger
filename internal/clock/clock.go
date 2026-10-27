// Package clock abstracts wall-clock time so business logic can be tested at fixed instants.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time in UTC.
type Clock interface {
	Now() time.Time
}

// System is the real clock.
type System struct{}

// Now is truncated to microseconds: PostgreSQL, MySQL and SQL Server store microseconds, and
// the audit and ledger hashes include timestamps, so what is hashed must be what is stored.
func (System) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

func NewFake(start time.Time) *Fake { return &Fake{t: start.UTC()} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
