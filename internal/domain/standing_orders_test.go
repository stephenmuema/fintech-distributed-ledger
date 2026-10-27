package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMonthlyStandingOrderKeepsItsDayAndFallsBackToShortMonthEnds(t *testing.T) {
	now := time.Date(2027, 1, 4, 9, 0, 0, 0, time.UTC)
	order, err := NewStandingOrder(uuid.New(), uuid.New(), "KES", 1_000, Monthly, NewDate(2027, 1, 31), nil, nil, NewDate(2027, 1, 4), now)
	if err != nil {
		t.Fatal(err)
	}
	var dues []Date
	for range 4 {
		dues = append(dues, order.NextRunDate)
		order.Record(NewStandingOrderExecution(order, now), now)
	}
	want := []Date{NewDate(2027, 1, 31), NewDate(2027, 2, 28), NewDate(2027, 3, 31), NewDate(2027, 4, 30)}
	for i := range want {
		if dues[i] != want[i] {
			t.Errorf("payment %d due %s, want %s", i+1, dues[i], want[i])
		}
	}
}

func TestStandingOrderCompletesAfterItsEndDateAndSuspendsAfterRepeatedDeclines(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	end := NewDate(2026, 11, 12)
	order, _ := NewStandingOrder(uuid.New(), uuid.New(), "KES", 1_000, Weekly, NewDate(2026, 10, 29), &end, nil, NewDate(2026, 10, 28), now)
	for range 3 {
		order.Record(NewStandingOrderExecution(order, now), now)
	}
	if order.Status != StandingCompleted {
		t.Errorf("after 29 Oct, 5 and 12 Nov: %s, next %s", order.Status, order.NextRunDate)
	}

	order, _ = NewStandingOrder(uuid.New(), uuid.New(), "KES", 1_000, Daily, NewDate(2026, 10, 29), nil, nil, NewDate(2026, 10, 28), now)
	suspended := false
	for i := range MaxStandingOrderFailures {
		e := NewStandingOrderExecution(order, now)
		e.Decline("insufficient_funds")
		suspended = order.Record(e, now)
		if suspended != (i == MaxStandingOrderFailures-1) {
			t.Fatalf("decline %d: suspended = %v", i+1, suspended)
		}
	}
	if order.Status != StandingSuspended || order.IsDue(NewDate(2030, 1, 1)) {
		t.Errorf("status %s; a suspended order must never be due", order.Status)
	}
}
