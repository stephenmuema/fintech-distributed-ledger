package domain

import (
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// CurrencyRate is treasury's mid rate for a currency, in units per US dollar (T24: CURRENCY).
// Rates are maintained under maker-checker; AsOf is when the change was authorised.
type CurrencyRate struct {
	ID       uuid.UUID       `db:"id" json:"id"`
	Version  int64           `db:"version" json:"version"`
	Currency string          `db:"currency" json:"currency"`
	PerUSD   decimal.Decimal `db:"per_usd" json:"perUsd"`
	AsOf     time.Time       `db:"as_of" json:"asOf"`
}

func (r *CurrencyRate) HistoryName() string { return "CurrencyRate" }
