package app

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
)

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// snake converts an enum name to snake_case for event payloads: PendingReview → pending_review.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mask hides all but the last four characters of an account number.
func mask(account string) string {
	if len(account) <= 4 {
		return account
	}
	return "***" + account[len(account)-4:]
}

// toUSD converts minor units to USD at mid-rate.
func (a *App) toUSD(amount int64, currency string) (decimal.Decimal, error) {
	ccy, err := domain.CurrencyOf(currency)
	if err != nil {
		return decimal.Zero, err
	}
	major := ccy.ToMajor(amount)
	if currency == "USD" {
		return major, nil
	}
	rate, err := a.Rates.MidRate(currency, "USD")
	if err != nil {
		return decimal.Zero, err
	}
	return major.Mul(rate), nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
