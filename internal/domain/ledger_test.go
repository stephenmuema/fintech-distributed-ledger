package domain

import (
	"regexp"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var (
	testNow   = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	testToday = NewDate(2026, 10, 28)
)

func mustAccount(t *testing.T, code, number string, typ AccountType, category int, currency string, allowNegative bool) *LedgerAccount {
	t.Helper()
	a, err := NewLedgerAccount(code+":"+NewID().String(), number, code, typ, category, currency, allowNegative, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wallet(t *testing.T, currency string) *LedgerAccount {
	return mustAccount(t, "wallet", "1000000018", Liability, CategoryMobileWallet, currency, false)
}

func float(t *testing.T, currency string) *LedgerAccount {
	return mustAccount(t, "float", currency+"140010001", Asset, CategoryProviderFloat, currency, true)
}

func revenue(t *testing.T, currency string) *LedgerAccount {
	return mustAccount(t, "rev", "PL52000"+currency, Revenue, CategoryFeeIncome, currency, true)
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := CodeOf(err); got != code {
		t.Fatalf("expected error code %q, got %q (%v)", code, got, err)
	}
}

func TestBalancedEntryUpdatesNormalBalancesAndRecordsRunningBalance(t *testing.T) {
	providerFloat, w, fees := float(t, "KES"), wallet(t, "KES"), revenue(t, "KES")

	entry, err := NewEntry("payment:1", CatCollection, "Collection").
		Debit(providerFloat, 10_000).Credit(w, 9_710).Credit(fees, 290).Post(testNow, testToday)
	if err != nil {
		t.Fatal(err)
	}

	if providerFloat.Balance != 10_000 || w.Balance != 9_710 || fees.Balance != 290 {
		t.Fatalf("balances: float %d wallet %d fees %d", providerFloat.Balance, w.Balance, fees.Balance)
	}
	if len(entry.Lines) != 3 {
		t.Fatalf("lines = %d", len(entry.Lines))
	}
	for _, l := range entry.Lines {
		if l.AccountID == w.ID && l.BalanceAfter != 9_710 {
			t.Fatalf("wallet balanceAfter = %d", l.BalanceAfter)
		}
		if l.BookingDate != testToday {
			t.Fatalf("booking date = %s", l.BookingDate)
		}
	}
}

func TestUnbalancedEntryIsRejected(t *testing.T) {
	_, err := NewEntry("bad", CatTransfer, "x").Debit(float(t, "KES"), 100).Credit(wallet(t, "KES"), 99).Post(testNow, testToday)
	expectCode(t, err, "unbalanced_entry")
}

func TestOverdraftIsRejectedWithoutMutatingAnyBalance(t *testing.T) {
	source, destination, providerFloat := wallet(t, "KES"), wallet(t, "KES"), float(t, "KES")
	if _, err := NewEntry("fund", CatCollection, "x").Debit(providerFloat, 500).Credit(source, 500).Post(testNow, testToday); err != nil {
		t.Fatal(err)
	}

	_, err := NewEntry("t", CatTransfer, "x").Debit(source, 501).Credit(destination, 501).Post(testNow, testToday)

	expectCode(t, err, "insufficient_funds")
	if source.Balance != 500 || destination.Balance != 0 {
		t.Fatalf("balances mutated: %d / %d", source.Balance, destination.Balance)
	}
}

func TestOverdraftCanBePermittedForASpecificAccount(t *testing.T) {
	merchant, held := wallet(t, "KES"), wallet(t, "KES")
	if _, err := NewEntry("dispute", CatDisputeHold, "x").Debit(merchant, 1_000).Credit(held, 1_000).Post(testNow, testToday, merchant); err != nil {
		t.Fatal(err)
	}
	if merchant.Balance != -1_000 {
		t.Fatalf("merchant balance = %d", merchant.Balance)
	}
}

func TestMultiCurrencyEntryMustBalanceInEachCurrency(t *testing.T) {
	usdWallet, kesWallet, usdFloat := wallet(t, "USD"), wallet(t, "KES"), float(t, "USD")
	if _, err := NewEntry("fund", CatCollection, "x").Debit(usdFloat, 10_000).Credit(usdWallet, 10_000).Post(testNow, testToday); err != nil {
		t.Fatal(err)
	}
	usdPosition := mustAccount(t, "pos", "USD180010001", Equity, CategoryFxPosition, "USD", true)
	kesPosition := mustAccount(t, "pos", "KES180010001", Equity, CategoryFxPosition, "KES", true)

	// Balanced in USD, balanced in KES.
	if _, err := NewEntry("fx:1", CatFxConversion, "x").
		Debit(usdWallet, 1_000).Credit(usdPosition, 1_000).
		Debit(kesPosition, 129_200).Credit(kesWallet, 129_200).Post(testNow, testToday); err != nil {
		t.Fatal(err)
	}

	// Debits equal credits overall, but not per currency → rejected.
	_, err := NewEntry("fx:2", CatFxConversion, "x").Debit(usdWallet, 1_000).Credit(kesWallet, 1_000).Post(testNow, testToday)
	expectCode(t, err, "unbalanced_entry")
}

func TestZeroLegsAreDroppedAndNegativeAmountsRejected(t *testing.T) {
	entry, err := NewEntry("p", CatCollection, "x").Debit(float(t, "KES"), 100).Credit(wallet(t, "KES"), 100).Credit(revenue(t, "KES"), 0).Post(testNow, testToday)
	if err != nil || len(entry.Lines) != 2 {
		t.Fatalf("entry=%v err=%v", entry, err)
	}

	_, err = NewEntry("n", CatCollection, "x").Debit(float(t, "KES"), -5).Credit(wallet(t, "KES"), 5).Post(testNow, testToday)
	expectCode(t, err, "invalid_amount")
}

func TestTransactionReferencesFollowT24Layout(t *testing.T) {
	entry, err := NewEntry("t", CatTransfer, "x").Debit(float(t, "KES"), 1).Credit(wallet(t, "KES"), 1).Post(testNow, NewDate(2026, 10, 13))
	if err != nil {
		t.Fatal(err)
	}
	// A book transfer's entry is AC…; FT… names the transaction record (TransactionRecordReference).
	if !regexp.MustCompile(`^AC26286[A-Z0-9]{5}$`).MatchString(entry.TransactionReference) {
		t.Fatalf("reference %q", entry.TransactionReference)
	}
	ft, err := TransactionRecordReference(NewDate(2026, 10, 13), 0)
	if err != nil || !regexp.MustCompile(`^FT26286[A-Z2-9]{5}$`).MatchString(ft) {
		t.Fatalf("record reference %q %v", ft, err)
	}
}

func TestSequencedReferencesAreUniqueAtVolumeAndKeepT24Layout(t *testing.T) {
	format := regexp.MustCompile(`^PC26301[A-Z2-9]{5}$`)
	seen := make(map[string]bool, 500_000)
	for n := range int64(500_000) { // 20× the 10k-a-day target, for one prefix on one day
		ref, err := SequencedTransactionReference(CatCollection, NewDate(2026, 10, 28), n)
		if err != nil {
			t.Fatal(err)
		}
		if seen[ref] {
			t.Fatalf("collision at sequence %d: %s", n, ref)
		}
		if n < 10 && !format.MatchString(ref) {
			t.Fatalf("bad layout %q", ref)
		}
		seen[ref] = true
	}
	if _, err := SequencedTransactionReference(CatCollection, NewDate(2026, 10, 28), 1<<25); err == nil {
		t.Fatal("expected the reference space to be bounded")
	}
}

func TestFeesRoundUpToTheMinorUnit(t *testing.T) {
	cases := []struct {
		amount   int64
		bps      int
		expected int64
	}{
		{10_000, 290, 290},
		{1, 290, 1},   // 0.029 rounds up to 1 minor unit
		{333, 150, 5}, // 4.995 → 5
		{0, 290, 0},
	}
	for _, c := range cases {
		if got := (FeeRule{BasisPoints: c.bps}).Calculate(c.amount); got != c.expected {
			t.Errorf("fee(%d @ %d bps) = %d, want %d", c.amount, c.bps, got, c.expected)
		}
	}
}

func TestCurrencyExponentsAreRespected(t *testing.T) {
	if got := MustCurrency("KES").ToMajor(123_456); !got.Equal(decimal.RequireFromString("1234.56")) {
		t.Errorf("KES major = %s", got)
	}
	if got := MustCurrency("UGX").ToMajor(123_456); !got.Equal(decimal.NewFromInt(123_456)) {
		t.Errorf("UGX major = %s", got)
	}
	if _, err := CurrencyOf("XYZ"); CodeOf(err) != "unsupported_currency" {
		t.Errorf("XYZ accepted: %v", err)
	}
	if got := MustCurrency("KES").Format(123_456_789); got != "1,234,567.89 KES" {
		t.Errorf("format = %q", got)
	}
}

func TestFxQuoteChargesExplicitSpreadAndTruncatesTarget(t *testing.T) {
	quote, err := NewFxQuote(NewID(), "USD", "KES", 10_000, decimal.RequireFromString("129.20"), 75, time.Minute, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Fee != 75 { // 0.75% of $100.00
		t.Errorf("fee = %d", quote.Fee)
	}
	if quote.TargetAmount != 1_282_310 { // $99.25 × 129.20 = KES 12,823.10
		t.Errorf("target = %d", quote.TargetAmount)
	}
	if quote.NetSourceAmount() != 9_925 {
		t.Errorf("net = %d", quote.NetSourceAmount())
	}
}

func TestAccountNumbersCarryAValidLuhnCheckDigit(t *testing.T) {
	number := CustomerAccountNumber(42)
	if len(number) != 10 || !IsValidCustomerAccountNumber(number) {
		t.Fatalf("invalid number %q", number)
	}
	tampered := number[:9] + string(rune('0'+(int(number[9]-'0')+1)%10))
	if IsValidCustomerAccountNumber(tampered) {
		t.Fatalf("tampered number %q accepted", tampered)
	}
	if got := InternalAccountNumber(CategoryProviderFloat, "KES", 3); got != "KES140010003" {
		t.Errorf("internal = %q", got)
	}
	if got := InternalAccountNumber(CategoryFeeIncome, "KES", 1); got != "PL52000KES" {
		t.Errorf("P&L = %q", got)
	}
}
