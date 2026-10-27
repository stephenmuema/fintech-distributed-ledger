package domain

import (
	"testing"
)

func suspenseFixture(t *testing.T, side SuspenseSide) (*SuspenseType, *LedgerAccount) {
	t.Helper()
	typ, err := NewSuspenseType("TESTSUSP", SuspenseTypeSpec{Name: "Test", Side: side, Currencies: []string{"kes"},
		AllowedTargets: []string{"internal"}, LocalFields: LocalFields{{Name: "ref", Label: "Ref", Type: FieldText, Required: true}}}, 14100, side.GLCode(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := NewLedgerAccountInGL("suspense:testsusp:KES", "KES141000001", "Test", side.AccountType(), 14100, side.GLCode(), "KES", false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return typ, acct
}

func TestSuspenseSpecIsNormalised(t *testing.T) {
	typ, _ := suspenseFixture(t, CreditSuspense)
	if typ.Currencies[0] != "KES" || typ.AllowedTargets[0] != "Internal" || typ.CobPolicy != PolicyWarn {
		t.Fatalf("not normalised: %+v", typ)
	}
	bad := []SuspenseTypeSpec{
		{Name: "", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}},
		{Name: "x", Side: "Sideways", Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"XXX"}, AllowedTargets: []string{"Internal"}},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}, WarnAfterDays: 5, EscalateAfterDays: 2},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}, CobPolicy: PolicyHaltCob},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}, WriteOffAfterDays: 10, WriteOffCategory: 14009},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}, AutoClearAt: 90},
		{Name: "x", Side: DebitSuspense, Currencies: []string{"KES"}, AllowedTargets: []string{"Internal"}, LocalFields: LocalFields{{Name: "Bad Name", Type: FieldText}}},
	}
	for i, spec := range bad {
		if err := spec.Normalize(); CodeOf(err) == "" {
			t.Fatalf("spec %d should be rejected: %v", i, err)
		}
	}
}

func TestSuspenseItemSettlesInParts(t *testing.T) {
	typ, acct := suspenseFixture(t, DebitSuspense)
	item, err := OpenSuspenseItem(typ, acct, 1_000, "R", "N", "manual", "", Attributes{"ref": "x"}, NewID(), NewDate(2026, 10, 28), "ops.amina", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := item.Settle(1_001, false, NewDate(2026, 10, 29)); CodeOf(err) != "invalid_clearing_amount" {
		t.Fatalf("over-clearing must fail: %v", err)
	}
	_ = item.Settle(400, false, NewDate(2026, 10, 29))
	if item.Status != ItemPartiallyCleared || item.OpenAmount != 600 {
		t.Fatalf("partial: %+v", item)
	}
	_ = item.Settle(600, true, NewDate(2026, 10, 30))
	if item.Status != ItemWrittenOff || item.ClosedOn == nil {
		t.Fatalf("written off: %+v", item)
	}
	if err := item.Settle(1, false, NewDate(2026, 10, 30)); CodeOf(err) != "suspense_item_closed" {
		t.Fatalf("closed item: %v", err)
	}
	if got := item.AgeDays(NewDate(2026, 11, 2)); got != 5 {
		t.Fatalf("age: %d", got)
	}
}

func TestClearingLegsFollowTheSide(t *testing.T) {
	if s, tg := CreditSuspense.ClearingLegs(); s != Debit || tg != Credit {
		t.Fatal("a credit suspense is debited and its owner credited")
	}
	if s, tg := DebitSuspense.ClearingLegs(); s != Credit || tg != Debit {
		t.Fatal("a debit suspense is credited and the owing account debited")
	}
}

func TestRaisedItemNeedsASecondOperator(t *testing.T) {
	typ, acct := suspenseFixture(t, DebitSuspense)
	if _, err := RaiseSuspenseItem(typ, acct, "KES140020001", 500, "", "n", nil, "ops.amina", NewDate(2026, 10, 28), testNow); CodeOf(err) != "local_field_required" {
		t.Fatalf("required local field: %v", err)
	}
	item, err := RaiseSuspenseItem(typ, acct, "KES140020001", 500, "", "n", Attributes{"ref": "A1"}, "ops.amina", NewDate(2026, 10, 28), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := item.AuthoriseRaise("OPS.AMINA", NewID(), NewDate(2026, 10, 28)); CodeOf(err) != "self_authorisation" {
		t.Fatalf("four eyes: %v", err)
	}
	if err := item.AuthoriseRaise("ops.brian", NewID(), NewDate(2026, 10, 29)); err != nil || item.Status != ItemOpen || item.OpenedOn != NewDate(2026, 10, 29) {
		t.Fatalf("authorise: %v %+v", err, item)
	}
}

func TestChartAllocatesTheLowestFreeCategory(t *testing.T) {
	chart := BuiltInChart().WithCategories([]CategoryDefinition{{14100, "a", "2510"}, {14102, "b", "1910"}})
	if code, _ := chart.NextFree(SuspenseCategoryFirst, SuspenseCategoryLast); code != 14101 {
		t.Fatalf("next free: %d", code)
	}
	if gl, _ := chart.GLFor(14102); gl != "1910" {
		t.Fatalf("runtime category GL: %s", gl)
	}
	if _, err := BuiltInChart().GLFor(14102); CodeOf(err) != "invalid_category" {
		t.Fatal("runtime categories are not built in")
	}
}

func TestNostroProofCloses(t *testing.T) {
	p := Proof{LedgerBalance: 50_000, StatementClosing: 48_750, UnmatchedStmtDebits: 1_250}
	p.Compute()
	if !p.Closes {
		t.Fatalf("proof: %+v", p)
	}
	p = Proof{LedgerBalance: 50_000, StatementClosing: 48_750, UnmatchedBookDebits: 1_000}
	p.Compute()
	if p.Closes || p.Difference != -250 {
		t.Fatalf("proof: %+v", p)
	}
}
