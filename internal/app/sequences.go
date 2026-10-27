package app

import (
	"fmt"
	"sync"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// sequenceBlocks hands out sequence numbers (journal and transaction references, customer and
// account numbers) from blocks reserved in their own short transaction. Units of work running in
// parallel then never queue on a sequence row from their first number to their commit. Numbers
// stay unique across instances; a block a process didn't use up leaves a gap, as T24's do.
type sequenceBlocks struct {
	mu     sync.Mutex
	blocks map[string]*sequenceBlock
}

type sequenceBlock struct{ next, end int64 }

const sequenceBlockSize = 50

// take returns the next number for key, reserving a new block with reserve when the current one
// is used up. reserve returns the first number of a block of n.
func (s *sequenceBlocks) take(key string, reserve func(n int64) (int64, error)) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.blocks[key]
	if b == nil || b.next >= b.end {
		start, err := reserve(sequenceBlockSize)
		if err != nil {
			return 0, err
		}
		if s.blocks == nil {
			s.blocks = map[string]*sequenceBlock{}
		}
		b = &sequenceBlock{start, start + sequenceBlockSize}
		s.blocks[key] = b
	}
	v := b.next
	b.next++
	return v, nil
}

// lockName takes a named lock when q is a unit of work (Tx.Lock): two units of work that check
// the same thing then check it one after the other. Reads outside a unit of work don't lock.
func lockName(q store.Q, name string) error {
	if tx, ok := q.(*store.Tx); ok {
		return tx.Lock(name)
	}
	return nil
}

// nextReferenceSequence takes the next value of a per-day, per-prefix counter. On a serial store
// it is taken in the unit of work (if another instance creates the same counter first, the unique
// key turns that into a retry); on a parallel one, from a block (sequenceBlocks).
func (a *App) nextReferenceSequence(tx *store.Tx, date domain.Date, prefix string) (int64, error) {
	if !tx.Serial() {
		return a.sequences.take("ref:"+date.String()+":"+prefix, func(n int64) (start int64, err error) {
			err = tx.Store().Independent(tx.Context(), func(q store.Q) error {
				start, err = advanceReferenceSequence(q, date, prefix, n)
				return err
			})
			return start, err
		})
	}
	return advanceReferenceSequence(tx, date, prefix, 1)
}

// advanceReferenceSequence reserves n values of a counter and returns the first.
func advanceReferenceSequence(q store.Q, date domain.Date, prefix string, n int64) (int64, error) {
	res, err := q.Exec("UPDATE reference_sequences SET next_value = next_value + ? WHERE booking_date = ? AND prefix = ?", n, date, prefix)
	if err != nil {
		return 0, err
	}
	if rows, err := res.RowsAffected(); err != nil || rows > 0 {
		if err != nil {
			return 0, err
		}
		return store.Scalar[int64](q, "SELECT next_value - ? FROM reference_sequences WHERE booking_date = ? AND prefix = ?", n, date, prefix)
	}
	if _, err := q.Exec("INSERT INTO reference_sequences (booking_date, prefix, next_value) VALUES (?, ?, ?)", date, prefix, n); err != nil {
		if store.IsUniqueViolation(err) {
			return 0, store.ErrConcurrency
		}
		return 0, err
	}
	return 0, nil
}

// reserveNumbers reserves n customer or account numbers and returns the first.
func reserveNumbers(q store.Q, name string, n int64) (int64, error) {
	res, err := q.Exec("UPDATE number_sequences SET next_value = next_value + ?, version = version + 1 WHERE name = ?", n, name)
	if err != nil {
		return 0, err
	}
	if rows, err := res.RowsAffected(); err != nil || rows == 0 {
		if err == nil {
			err = fmt.Errorf("number sequence '%s' is not initialised", name)
		}
		return 0, err
	}
	return store.Scalar[int64](q, "SELECT next_value - ? FROM number_sequences WHERE name = ?", n, name)
}
