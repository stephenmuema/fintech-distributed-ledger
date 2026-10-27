package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// TableRates are the production FX rates: treasury inputs mid rates per US dollar, a checker
// authorises them, and quotes are refused once a rate is older than the configured maximum
// age. Limits and reports keep converting with the latest rate whatever its age, so a missed
// update never stops payouts. The sandbox uses the static simulated table instead.
type TableRates struct {
	seed  map[string]decimal.Decimal
	mu    sync.RWMutex
	rates map[string]domain.CurrencyRate
}

// NewTableRates seeds an empty table from seed (as of the epoch, so FX quoting waits for
// treasury's first update while limits work at once).
func NewTableRates(seed map[string]decimal.Decimal) *TableRates {
	return &TableRates{seed: seed, rates: map[string]domain.CurrencyRate{}}
}

func (t *TableRates) MidRate(from, to string) (decimal.Decimal, error) {
	if from == to {
		return decimal.NewFromInt(1), nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	f, ok1 := t.rates[from]
	g, ok2 := t.rates[to]
	if !ok1 || !ok2 || f.PerUSD.IsZero() {
		return decimal.Zero, domain.Rule("rate_unavailable", "No rate for %s/%s.", from, to)
	}
	return g.PerUSD.Div(f.PerUSD), nil
}

// AsOf is the oldest rate's time: how current the whole table is.
func (t *TableRates) AsOf() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var oldest time.Time
	for _, r := range t.rates {
		if oldest.IsZero() || r.AsOf.Before(oldest) {
			oldest = r.AsOf
		}
	}
	return oldest
}

// PairAsOf is when the older of a pair's two rates was set.
func (t *TableRates) PairAsOf(from, to string) time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	a, b := t.rates[from].AsOf, t.rates[to].AsOf
	if from == "USD" {
		return b
	}
	if to == "USD" || a.Before(b) {
		return a
	}
	return b
}

func (t *TableRates) load(q store.Q) error {
	rows, err := store.Select[domain.CurrencyRate](q, "SELECT * FROM currency_rates ORDER BY currency")
	if err != nil {
		return err
	}
	rates := map[string]domain.CurrencyRate{}
	for _, r := range rows {
		rates[r.Currency] = r
	}
	t.mu.Lock()
	t.rates = rates
	t.mu.Unlock()
	return nil
}

// ensureRates seeds the rate table on first start and loads it (production rates only).
func (a *App) ensureRates(tx *store.Tx) error {
	table, ok := a.Rates.(*TableRates)
	if !ok {
		return nil
	}
	exists, err := store.Exists(tx, "SELECT 1 FROM currency_rates")
	if err != nil {
		return err
	}
	if !exists {
		for ccy, perUSD := range table.seed {
			if err := tx.Insert("currency_rates", &domain.CurrencyRate{ID: domain.NewID(), Currency: ccy, PerUSD: perUSD, AsOf: time.Unix(0, 0).UTC()}); err != nil {
				return err
			}
		}
		a.Logger.Warn("FX rates seeded as stale: FX quotes are refused until treasury inputs and a checker authorises current rates")
	}
	return table.load(tx)
}

// checkRateAge refuses an FX quote on a rate older than the configured maximum age.
func (a *App) checkRateAge(from, to string) error {
	table, ok := a.Rates.(*TableRates)
	if !ok || a.Options.FxMaxRateAge <= 0 {
		return nil
	}
	asOf := table.PairAsOf(from, to)
	if a.now().Sub(asOf) > a.Options.FxMaxRateAge {
		return domain.Rule("rate_stale", "The %s/%s rate is older than %s; treasury must update it before FX can be quoted.", from, to, a.Options.FxMaxRateAge)
	}
	return nil
}

// RatesChange is treasury's input: units of each currency per US dollar.
type RatesChange struct {
	Rates map[string]decimal.Decimal `json:"rates"`
}

// CurrencyRateView is a rate with its age, for treasury.
type CurrencyRateView struct {
	domain.CurrencyRate
	AgeMinutes int64 `json:"ageMinutes"`
	Stale      bool  `json:"stale"`
}

func (a *App) CurrencyRates(ctx context.Context) ([]CurrencyRateView, error) {
	rows, err := store.Select[domain.CurrencyRate](a.read(ctx), "SELECT * FROM currency_rates ORDER BY currency")
	if err != nil {
		return nil, err
	}
	out := make([]CurrencyRateView, len(rows))
	for i, r := range rows {
		age := a.now().Sub(r.AsOf)
		out[i] = CurrencyRateView{CurrencyRate: r, AgeMinutes: int64(age.Minutes()), Stale: a.Options.FxMaxRateAge > 0 && age > a.Options.FxMaxRateAge}
	}
	return out, nil
}

// ProposeRates inputs new mid rates (INAU); they apply, stamped with the authorisation time,
// when a checker authorises them.
func (p *Parameters) ProposeRates(ctx context.Context, change RatesChange) (*domain.ParameterChangeRequest, error) {
	if _, ok := p.a.Rates.(*TableRates); !ok {
		return nil, domain.Rule("rates_simulated", "The sandbox uses simulated rates.")
	}
	if len(change.Rates) == 0 {
		return nil, domain.Rule("invalid_rates", "Give at least one rate.")
	}
	normalised := map[string]decimal.Decimal{}
	for ccy, perUSD := range change.Rates {
		c, err := domain.CurrencyOf(strings.ToUpper(ccy))
		if err != nil {
			return nil, err
		}
		if !perUSD.IsPositive() || (c.Code == "USD" && !perUSD.Equal(decimal.NewFromInt(1))) {
			return nil, domain.Rule("invalid_rates", "%s must be a positive number of units per US dollar (USD is 1).", c.Code)
		}
		normalised[c.Code] = perUSD
	}
	return p.propose(ctx, domain.ParamCurrencyRates, "FX", RatesChange{normalised})
}

func (a *App) applyRates(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var change RatesChange
	if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
		return err
	}
	for ccy, perUSD := range change.Rates {
		rate, err := store.Get[domain.CurrencyRate](tx, "SELECT * FROM currency_rates WHERE currency = ?", ccy)
		if err != nil {
			return err
		}
		if rate == nil {
			if err := tx.Insert("currency_rates", &domain.CurrencyRate{ID: domain.NewID(), Currency: ccy, PerUSD: perUSD, AsOf: tx.Now()}); err != nil {
				return err
			}
			continue
		}
		rate.PerUSD, rate.AsOf = perUSD, tx.Now()
		if err := tx.Update("currency_rates", rate); err != nil {
			return err
		}
	}
	return tx.Audit("fx.rates_updated", "currency_rates", "FX", change)
}

// outflowToday is the customer money that left (or is on its way out) today, in minor units of
// the local currency: payout reserves, channel transfers out and refunds, the debits on
// customer accounts.
func (a *App) outflowToday(tx *store.Tx) (int64, error) {
	today, err := a.Dates.OnlineBookingDate(tx)
	if err != nil {
		return 0, err
	}
	rows, err := store.Select[struct {
		Currency string `db:"currency"`
		Total    int64  `db:"total"`
	}](tx, `SELECT l.currency AS currency, COALESCE(SUM(l.amount), 0) AS total FROM journal_lines l
	    JOIN journal_entries e ON e.id = l.journal_entry_id JOIN ledger_accounts a ON a.id = l.account_id
	    WHERE e.booking_date = ? AND e.category IN (?, ?, ?) AND l.direction = ? AND a.category < 10000 GROUP BY l.currency`,
		today, domain.CatPayoutReserve, domain.CatChannelTransfer, domain.CatRefund, domain.Debit)
	if err != nil {
		return 0, err
	}
	local, err := domain.CurrencyOf(a.Options.LocalCurrency)
	if err != nil {
		return 0, err
	}
	total := decimal.Zero
	for _, r := range rows {
		ccy, err := domain.CurrencyOf(r.Currency)
		if err != nil {
			return 0, err
		}
		rate, err := a.Rates.MidRate(r.Currency, local.Code)
		if err != nil {
			return 0, err
		}
		total = total.Add(ccy.ToMajor(r.Total).Mul(rate))
	}
	return local.ToMinorTruncated(total), nil
}

// overOutflowLimit reports whether amount (in currency) would take today's outflow past the
// platform-wide daily limit: a brake on a compromised API key draining customer accounts.
func (a *App) overOutflowLimit(tx *store.Tx, amount int64, currency string) (bool, error) {
	if a.Options.DailyOutflowLimit <= 0 {
		return false, nil
	}
	// Today's platform-wide outflow is checked by one unit of work at a time.
	if err := tx.Lock("outflow"); err != nil {
		return false, err
	}
	sent, err := a.outflowToday(tx)
	if err != nil {
		return false, err
	}
	rate, err := a.Rates.MidRate(currency, a.Options.LocalCurrency)
	if err != nil {
		return false, err
	}
	from, err := domain.CurrencyOf(currency)
	if err != nil {
		return false, err
	}
	local, _ := domain.CurrencyOf(a.Options.LocalCurrency)
	return sent+local.ToMinorTruncated(from.ToMajor(amount).Mul(rate)) > a.Options.DailyOutflowLimit, nil
}
