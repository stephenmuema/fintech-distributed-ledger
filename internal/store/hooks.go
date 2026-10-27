package store

import (
	"context"

	"github.com/google/uuid"
)

// A business operation can span several units of work (a payment is created, then routed to a
// provider, then confirmed). A Binding ties every unit of work of one operation to the
// transaction record it executes: the application opens the record in the first unit of work
// that writes a bound row, so the record exists if and only if the operation wrote something.

// Binding is the transaction record an operation executes.
type Binding struct {
	ID uuid.UUID
	// Open creates or updates the record. It runs once per unit of work, before the first bound
	// row is written, and must be idempotent across units of work (a later one finds it there).
	Open func(tx *Tx) error
	// System: the core opened it for a row it books on its own (a reported deposit); the next
	// such row in the same unit of work gets its own.
	System bool
}

type bindingKey struct{}

// WithBinding binds every unit of work run with ctx to a transaction record.
func WithBinding(ctx context.Context, b *Binding) context.Context {
	return context.WithValue(ctx, bindingKey{}, b)
}

// BindingOf returns the binding carried by ctx, or nil.
func BindingOf(ctx context.Context) *Binding {
	b, _ := ctx.Value(bindingKey{}).(*Binding)
	return b
}

// Bind binds the rest of this unit of work (a job that executes several operations in one
// commit binds each in turn).
func (t *Tx) Bind(b *Binding) {
	t.binding, t.opened = b, false
}

// Binding returns the transaction record this unit of work is bound to, or nil.
func (t *Tx) Binding() *Binding { return t.binding }

// OpenBinding runs the binding's Open once in this unit of work and returns the record's id
// (uuid.Nil when unbound).
func (t *Tx) OpenBinding() (uuid.UUID, error) {
	if t.binding == nil {
		return uuid.Nil, nil
	}
	if !t.opened {
		t.opened = true
		if t.binding.Open != nil {
			if err := t.binding.Open(t); err != nil {
				return uuid.Nil, err
			}
		}
	}
	return t.binding.ID, nil
}

// Hook follows writes to a table inside the unit of work that makes them.
type Hook struct {
	// BeforeInsert may change the row before it is written.
	BeforeInsert func(tx *Tx, row any) error
	// AfterWrite runs after every insert or update of the table.
	AfterWrite func(tx *Tx, row any) error
}

// Hook registers a hook for a table. Hooks are registered while the application is built,
// before any unit of work runs.
func (s *Store) Hook(table string, h Hook) {
	if s.hooks == nil {
		s.hooks = map[string]Hook{}
	}
	s.hooks[table] = h
}
