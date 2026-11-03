package domain

import (
	"math"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

func init() {
	// Rates and LCY amounts are JSON numbers (exact decimals), not strings.
	decimal.MarshalJSONWithoutQuotes = true
}

// Currency is a supported fiat currency or settlement asset with its minor-unit exponent. All money in Zirnis is held
// as int64 minor units (cents, or shillings for UGX) — never floating point.
type Currency struct {
	Code     string
	Exponent int32
}

var supportedCurrencies = map[string]Currency{
	"KES":  {"KES", 2},
	"UGX":  {"UGX", 0},
	"TZS":  {"TZS", 2},
	"NGN":  {"NGN", 2},
	"USD":  {"USD", 2},
	"EUR":  {"EUR", 2},
	"GBP":  {"GBP", 2},
	"USDC": {"USDC", 6},
}

// SupportedCurrencies returns the supported codes in alphabetical order.
func SupportedCurrencies() []string {
	codes := make([]string, 0, len(supportedCurrencies))
	for c := range supportedCurrencies {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return codes
}

func IsSupportedCurrency(code string) bool {
	_, ok := supportedCurrencies[code]
	return ok
}

// CurrencyOf looks up a supported currency.
func CurrencyOf(code string) (Currency, error) {
	if c, ok := supportedCurrencies[code]; ok {
		return c, nil
	}
	return Currency{}, Rule("unsupported_currency", "Currency '%s' is not supported.", code)
}

// MustCurrency is CurrencyOf for codes already validated.
func MustCurrency(code string) Currency {
	c, err := CurrencyOf(code)
	if err != nil {
		panic(err)
	}
	return c
}

func (c Currency) ToMajor(minor int64) decimal.Decimal {
	return decimal.New(minor, -c.Exponent)
}

// ToMinorTruncated converts a major-unit amount to minor units, rounding toward zero.
func (c Currency) ToMinorTruncated(major decimal.Decimal) int64 {
	return major.Shift(c.Exponent).Truncate(0).IntPart()
}

// ParseMajor converts a provider's major-unit amount ("500", "500.00", 500.5) to minor units,
// rejecting values with more precision than the currency has — money is never rounded silently.
func (c Currency) ParseMajor(s string) (int64, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return 0, Rule("invalid_amount", "'%s' is not an amount.", s)
	}
	minor := d.Shift(c.Exponent)
	if !minor.Equal(minor.Truncate(0)) {
		return 0, Rule("invalid_amount", "'%s' has more decimals than %s allows.", s, c.Code)
	}
	if minor.GreaterThan(decimal.NewFromInt(math.MaxInt64)) || minor.LessThan(decimal.NewFromInt(math.MinInt64)) {
		return 0, Rule("invalid_amount", "'%s' exceeds the supported amount range.", s)
	}
	return minor.IntPart(), nil
}

// MajorString renders minor units as a plain major-unit string, e.g. 50000 → "500.00".
func (c Currency) MajorString(minor int64) string { return c.ToMajor(minor).StringFixed(c.Exponent) }

// Format renders an amount like "1,234.56 KES".
func (c Currency) Format(minor int64) string {
	return FormatNumber(c.ToMajor(minor), c.Exponent) + " " + c.Code
}

// FormatNumber renders d with thousands separators and the given decimal places
// (rounding half away from zero, like .NET's "N" format).
func FormatNumber(d decimal.Decimal, places int32) string {
	s := d.Round(places).StringFixed(places)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if frac != "" {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return sign + b.String()
}

// FeeRule is a percentage in basis points plus a fixed minor-unit component. Percentage fees
// round UP to the next minor unit so the platform never under-collects due to rounding.
type FeeRule struct {
	BasisPoints int   `json:"basisPoints"`
	FixedMinor  int64 `json:"fixedMinor"`
}

func (f FeeRule) Calculate(amountMinor int64) int64 {
	if amountMinor <= 0 {
		return 0
	}
	bps := int64(f.BasisPoints)
	whole, rest := amountMinor/10_000, amountMinor%10_000
	variable := whole*bps + (rest*bps+9_999)/10_000
	return variable + f.FixedMinor
}
