package domain

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var testHolidays = Holidays{NewDate(2026, 10, 20): true, NewDate(2026, 12, 25): true, NewDate(2026, 12, 26): true}

func TestFridayRollsToMondayAndIsMonthEndWhenCrossingMonths(t *testing.T) {
	dates := InitialiseSystemDates(NewDate(2026, 10, 30), testHolidays)

	if dates.NextWorkingDay != NewDate(2026, 11, 2) {
		t.Errorf("next = %s", dates.NextWorkingDay)
	}
	if !dates.IsMonthEnd() {
		t.Error("expected month end")
	}
	if dates.OnlineBookingDate() != dates.Today {
		t.Error("online booking date should be today")
	}
}

func TestHolidaysAreSkippedAndWeekendStartMovesToNextWorkingDay(t *testing.T) {
	if got := NextWorkingDayAfter(NewDate(2026, 10, 19), testHolidays); got != NewDate(2026, 10, 21) { // Mashujaa Day
		t.Errorf("after 19 Oct = %s", got)
	}
	if got := InitialiseSystemDates(NewDate(2026, 10, 24), testHolidays).Today; got != NewDate(2026, 10, 26) { // Saturday
		t.Errorf("Saturday start = %s", got)
	}
}

func TestDuringCobOnlinePostingsBookToNextWorkingDayAndSodRollsDates(t *testing.T) {
	dates := InitialiseSystemDates(NewDate(2026, 10, 28), testHolidays)

	if err := dates.EnterCob(NewID()); err != nil {
		t.Fatal(err)
	}
	if got := dates.OnlineBookingDate(); got != NewDate(2026, 10, 29) {
		t.Errorf("booking date during COB = %s", got)
	}
	expectCode(t, dates.EnterCob(NewID()), "cob_in_progress")

	dates.RollToNextWorkingDay(testHolidays)
	if dates.Today != NewDate(2026, 10, 29) || dates.LastWorkingDay != NewDate(2026, 10, 28) || dates.Status != StatusOnline {
		t.Errorf("after SOD: %+v", dates)
	}
}

func TestInterestAccruesActual365Truncated(t *testing.T) {
	cases := []struct {
		balance  int64
		rate     string
		days     int
		expected int64
	}{
		{1_000_000, "0.04", 1, 109}, // KES 10,000 @ 4% for one day = 1.0958 → 1.09
		{1_000_000, "0.04", 3, 328}, // weekend accrual
		{-5_000, "0.04", 1, 0},      // no interest on negative balances
	}
	for _, c := range cases {
		if got := CalculateInterest(c.balance, decimal.RequireFromString(c.rate), c.days); got != c.expected {
			t.Errorf("interest(%d, %s, %d) = %d, want %d", c.balance, c.rate, c.days, got, c.expected)
		}
	}
}

func TestInputterCannotAuthoriseTheirOwnRecord(t *testing.T) {
	adjustment, err := NewManualAdjustment("KES", "Correct misposting", []AdjustmentLine{
		{"KES140090001", Debit, 100}, {"KES140020001", Credit, 100},
	}, "ops.amina", testNow)
	if err != nil {
		t.Fatal(err)
	}

	expectCode(t, adjustment.Authorise("OPS.AMINA", NewID(), testNow), "self_authorisation")
	if err := adjustment.Authorise("ops.brian", NewID(), testNow); err != nil {
		t.Fatal(err)
	}
	if adjustment.Status != Authorised {
		t.Errorf("status = %s", adjustment.Status)
	}
	expectCode(t, adjustment.Reject("ops.chen", "late", testNow), "not_unauthorised")
}

func TestUnbalancedAdjustmentCannotEvenBeInput(t *testing.T) {
	_, err := NewManualAdjustment("KES", "Bad", []AdjustmentLine{{"A", Debit, 100}, {"B", Credit, 90}}, "ops.amina", testNow)
	expectCode(t, err, "unbalanced_entry")
}

func newPayout(t *testing.T, decision RiskDecision) *Payout {
	t.Helper()
	dest, err := NewPayoutDestination(RailMobileMoney, "+254700000001", "Wanjiru", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPayout(NewID(), NewID(), 10_000, 100, "KES", dest, nil, decision, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRiskDecisionSetsTheInitialPayoutState(t *testing.T) {
	if s := newPayout(t, RiskApprove).Status; s != PayoutQueued {
		t.Errorf("approve → %s", s)
	}
	if s := newPayout(t, RiskReview).Status; s != PayoutPendingReview {
		t.Errorf("review → %s", s)
	}
	rejected := newPayout(t, RiskDecline)
	if rejected.Status != PayoutRejected || rejected.HoldsFunds() {
		t.Errorf("decline → %s (holds funds %v)", rejected.Status, rejected.HoldsFunds())
	}
}

func TestPayoutRetriesBackOffExponentiallyThenGiveUp(t *testing.T) {
	payout := newPayout(t, RiskApprove)

	for i, wait := range []time.Duration{30 * time.Second, 60 * time.Second} {
		ok, err := payout.ScheduleRetry("provider_unavailable", testNow)
		if err != nil || !ok {
			t.Fatalf("retry %d: ok=%v err=%v", i, ok, err)
		}
		if !payout.NextAttemptAt.Equal(testNow.Add(wait)) {
			t.Errorf("retry %d next = %s", i, payout.NextAttemptAt)
		}
	}
	for {
		ok, err := payout.ScheduleRetry("provider_unavailable", testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	if payout.SubmissionAttempts != MaxPayoutSubmissionAttempts {
		t.Errorf("attempts = %d", payout.SubmissionAttempts)
	}
}

func TestPaidPayoutsCannotBeFailed(t *testing.T) {
	payout := newPayout(t, RiskApprove)
	if err := payout.MarkSubmitted("pesalink", "PSL1", testNow); err != nil {
		t.Fatal(err)
	}
	if err := payout.MarkPaid(testNow); err != nil {
		t.Fatal(err)
	}
	expectCode(t, payout.MarkFailed("late_failure", testNow), "invalid_payout_state")
}

func riskContext(usd int64, velocity int, blocked bool, ageHours float64) RiskContext {
	return RiskContext{
		CustomerID: NewID(), Operation: "payout", Amount: 1, Currency: "KES", AmountUSD: decimal.NewFromInt(usd),
		Counterparty: Ptr("+254700000001"), CustomerAge: time.Duration(ageHours * float64(time.Hour)),
		OutgoingCountLastHour: velocity, CounterpartyBlocked: blocked, CounterpartySeenBefore: true,
	}
}

func TestRiskScoresMapToDecisions(t *testing.T) {
	cases := []struct {
		name     string
		ctx      RiskContext
		expected RiskDecision
	}{
		{"ordinary", riskContext(100, 0, false, 72), RiskApprove},
		{"large + new customer = 60", riskContext(6_000, 0, false, 2), RiskReview},
		{"blocklisted", riskContext(100, 0, true, 72), RiskDecline},
		{"velocity", riskContext(100, 12, false, 72), RiskDecline},
	}
	for _, c := range cases {
		if got := Assess(c.ctx, DefaultRiskRules, testNow).Decision; got != c.expected {
			t.Errorf("%s: %s, want %s", c.name, got, c.expected)
		}
	}
}

func TestReconciliationClassifiesEveryBreak(t *testing.T) {
	report := []SettlementReportLine{
		{"A", ReconCharge, 1_000, 29, "KES"},
		{"B", ReconCharge, 2_500, 70, "KES"},
		{"C", ReconCharge, 700, 20, "KES"},
	}
	ledger := []LedgerRecord{
		{"A", ReconCharge, 1_000, "KES"},
		{"B", ReconCharge, 2_000, "KES"},
		{"D", ReconPayout, 900, "KES"},
	}

	run, err := ExecuteReconciliation("acquirer-a", testToday, report, ledger, "ops.amina", testNow)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]ReconOutcome{"A": ReconMatched, "B": ReconAmountMismatch, "C": ReconMissingInLedger, "D": ReconMissingInReport}
	for _, item := range run.Items {
		if want[item.ProviderReference] != item.Outcome {
			t.Errorf("%s: %s, want %s", item.ProviderReference, item.Outcome, want[item.ProviderReference])
		}
	}
	if run.OpenExceptionCount() != 3 {
		t.Errorf("open exceptions = %d", run.OpenExceptionCount())
	}
	expectCode(t, run.MarkSettled(NewID(), testNow), "unresolved_exceptions")
}
