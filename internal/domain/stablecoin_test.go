package domain

import (
	"math"
	"testing"
)

func TestUSDCAmountsAreExactAndBounded(t *testing.T) {
	c := MustCurrency("USDC")
	if c.Exponent != 6 {
		t.Fatal(c)
	}
	for _, tt := range []struct {
		major string
		want  int64
	}{
		{"0.000001", 1}, {"123.456789", 123456789}, {"9223372036854.775807", math.MaxInt64},
	} {
		got, err := c.ParseMajor(tt.major)
		if err != nil || got != tt.want {
			t.Fatalf("%s: %d %v", tt.major, got, err)
		}
		if c.MajorString(got) != tt.major {
			t.Fatalf("round trip: %s", c.MajorString(got))
		}
	}
	for _, invalid := range []string{"0.0000001", "9223372036854.775808", "18446744073709.551617", "-9223372036854.775809"} {
		if _, err := c.ParseMajor(invalid); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestUSDAndUSDCCannotBalanceEachOther(t *testing.T) {
	_, err := NewEntry("different-assets", CatTransfer, "Distinct assets").Debit(float(t, "USD"), 100).Credit(wallet(t, "USDC"), 100).Post(testNow, testToday)
	expectCode(t, err, "unbalanced_entry")
}

func TestStablecoinProviderCapabilities(t *testing.T) {
	c := ProviderCapabilities{Collections: []ProviderCollection{{Method: MethodStablecoin, Currencies: []string{"USDC"}}}, CheckCharge: true, CheckTransaction: true}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Collects(MethodStablecoin, "USDC"); !ok {
		t.Fatal("missing stablecoin capability")
	}
}
