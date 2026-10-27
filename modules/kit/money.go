package kit

import (
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// Currency is an ISO 4217 currency with its minor-unit exponent. The core moves money in minor
// units; providers usually speak major units ("500.00"), so modules convert at the edge.
type Currency struct {
	Code     string
	Exponent int32
}

var currencies = map[string]Currency{
	"KES": {"KES", 2}, "UGX": {"UGX", 0}, "TZS": {"TZS", 2}, "NGN": {"NGN", 2},
	"USD": {"USD", 2}, "EUR": {"EUR", 2}, "GBP": {"GBP", 2},
}

// CurrencyOf returns a currency by code.
func CurrencyOf(code string) (Currency, error) {
	if c, ok := currencies[strings.ToUpper(code)]; ok {
		return c, nil
	}
	return Currency{}, fmt.Errorf("currency %q is not supported", code)
}

// MustCurrency is CurrencyOf for codes already validated.
func MustCurrency(code string) Currency {
	c, err := CurrencyOf(code)
	if err != nil {
		panic(err)
	}
	return c
}

// ToMajor converts minor units to a major-unit decimal.
func (c Currency) ToMajor(minor int64) decimal.Decimal { return decimal.New(minor, -c.Exponent) }

// ParseMajor converts a provider's major-unit amount ("500", "500.00") to minor units, rejecting
// values with more precision than the currency has — money is never rounded silently.
func (c Currency) ParseMajor(s string) (int64, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	minor := d.Shift(c.Exponent)
	if !minor.Equal(minor.Truncate(0)) {
		return 0, fmt.Errorf("%q has more decimals than %s allows", s, c.Code)
	}
	return minor.IntPart(), nil
}

// MajorString renders minor units as a plain major-unit string, e.g. 50000 → "500.00".
func (c Currency) MajorString(minor int64) string { return c.ToMajor(minor).StringFixed(c.Exponent) }

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }
