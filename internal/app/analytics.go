package app

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Analytics is business intelligence over transactions and customers: how much moved, through
// what, for whom, and how that compares with the period before. Every figure is an aggregate;
// the only names it shows are businesses' (top merchants), never an individual's.
//
// Days are booking dates (the accounting truth, portable across engines). A range of two days
// or less is shown by the hour the transaction was input instead.
type Analytics struct {
	From         domain.Date `json:"from"`
	To           domain.Date `json:"to"`
	PreviousFrom domain.Date `json:"previousFrom"`
	PreviousTo   domain.Date `json:"previousTo"`
	Currency     string      `json:"currency"`
	Currencies   []string    `json:"currencies"` // currencies with activity in the range
	Granularity  string      `json:"granularity"`

	KPIs     AnalyticsKPIs `json:"kpis"`
	Previous AnalyticsKPIs `json:"previous"`

	Series []AnalyticsBucket `json:"series"`

	ByFlow     []Breakdown `json:"byFlow"`
	ByChannel  []Breakdown `json:"byChannel"`
	ByType     []Breakdown `json:"byType"`
	BySegment  []Breakdown `json:"bySegment"` // Individual or Business: the customer who initiated, or was paid
	ByProvider []Breakdown `json:"byProvider"`
	Declines   []Breakdown `json:"declines"`

	TopMerchants []MerchantRank `json:"topMerchants"`

	Customers struct {
		Total  int              `json:"total"`
		ByType map[string]int   `json:"byType"`
		ByKyc  map[string]int   `json:"byKyc"`
		ByTier map[string]int   `json:"byTier"`
		New    []AnalyticsCount `json:"new"` // new customers per bucket
	} `json:"customers"`
	Accounts struct {
		ByCurrency    map[string]int `json:"byCurrency"`
		Sub           int            `json:"sub"`
		MultiCurrency int            `json:"multiCurrency"` // customers with open main accounts in more than one currency
	} `json:"accounts"`
	AsOf time.Time `json:"asOf"`
}

// AnalyticsKPIs are the headline figures for a period. Counts cover every currency; values are
// in the selected currency, in minor units.
type AnalyticsKPIs struct {
	Transactions    int     `json:"transactions"`
	Completed       int     `json:"completed"`
	Failed          int     `json:"failed"`
	SuccessRate     float64 `json:"successRate"` // completed / (completed + failed)
	Value           int64   `json:"value"`
	AverageTicket   int64   `json:"averageTicket"`
	Fees            int64   `json:"fees"`
	ActiveCustomers int     `json:"activeCustomers"`
	NewCustomers    int     `json:"newCustomers"`
	NewAccounts     int     `json:"newAccounts"`
}

// AnalyticsBucket is one day or hour.
type AnalyticsBucket struct {
	Bucket    string `json:"bucket"` // YYYY-MM-DD, or an RFC 3339 hour
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
	Other     int    `json:"other"`
	Value     int64  `json:"value"`
	Fees      int64  `json:"fees"`
}

// Breakdown is one value of a dimension.
type Breakdown struct {
	Key       string `json:"key" db:"k"`
	Count     int    `json:"count" db:"n"`
	Completed int    `json:"completed" db:"ok"`
	Failed    int    `json:"failed" db:"bad"`
	Value     int64  `json:"value" db:"v"`
}

// MerchantRank is a business by completed collections.
type MerchantRank struct {
	CustomerID string `json:"customerId" db:"id"`
	Name       string `json:"name" db:"name"`
	Count      int    `json:"count" db:"n"`
	Value      int64  `json:"value" db:"v"`
}

// AnalyticsCount is a count in a bucket.
type AnalyticsCount struct {
	Bucket string `json:"bucket"`
	Count  int    `json:"count"`
}

// AnalyticsQuery selects the period and the currency values are shown in.
type AnalyticsQuery struct {
	From, To *domain.Date
	Currency string
}

// MaxAnalyticsDays bounds a range.
const MaxAnalyticsDays = 366

func (a *App) Analytics(ctx context.Context, query AnalyticsQuery) (*Analytics, error) {
	q := a.read(ctx)
	today, err := a.Dates.OnlineBookingDate(q)
	if err != nil {
		return nil, err
	}
	to := today
	if query.To != nil {
		to = *query.To
	}
	from := to.AddDays(-29)
	if query.From != nil {
		from = *query.From
	}
	if to.Before(from) {
		return nil, domain.Rule("invalid_range", "The range ends before it starts.")
	}
	days := int(to.Time().Sub(from.Time()).Hours()/24) + 1
	if days > MaxAnalyticsDays {
		return nil, domain.Rule("invalid_range", "A range is at most %d days.", MaxAnalyticsDays)
	}
	currency := query.Currency
	if currency == "" {
		currency = "KES"
	}
	if !domain.IsSupportedCurrency(currency) {
		return nil, domain.Rule("unsupported_currency", "Currency '%s' is not supported.", currency)
	}

	r := &Analytics{From: from, To: to, PreviousTo: from.AddDays(-1), PreviousFrom: from.AddDays(-days), Currency: currency,
		Granularity: "day", AsOf: a.now().UTC()}
	if days <= 2 {
		r.Granularity = "hour"
	}
	period := []any{from.String(), to.String()}
	if r.KPIs, err = a.kpis(q, from, to, currency); err != nil {
		return nil, err
	}
	if r.Previous, err = a.kpis(q, r.PreviousFrom, r.PreviousTo, currency); err != nil {
		return nil, err
	}
	if r.Series, err = a.series(q, from, to, currency, r.Granularity); err != nil {
		return nil, err
	}

	// Breakdowns: count and outcomes over every currency, value in the selected one.
	measures := "COUNT(*) AS n, SUM(CASE WHEN t.status = ? THEN 1 ELSE 0 END) AS ok, SUM(CASE WHEN t.status = ? THEN 1 ELSE 0 END) AS bad, " +
		"COALESCE(SUM(CASE WHEN t.status = ? AND t.currency = ? THEN t.amount ELSE 0 END), 0) AS v"
	args := func(extra ...any) []any {
		return append(append([]any{domain.TxnCompleted, domain.TxnFailed, domain.TxnCompleted, currency}, period...), extra...)
	}
	for _, d := range []struct {
		target *[]Breakdown
		sql    string
		args   []any
	}{
		{&r.ByFlow, "SELECT t.flow AS k, " + measures + " FROM transactions t WHERE t.booking_date BETWEEN ? AND ? GROUP BY t.flow", args()},
		{&r.ByChannel, "SELECT t.channel AS k, " + measures + " FROM transactions t WHERE t.booking_date BETWEEN ? AND ? GROUP BY t.channel", args()},
		{&r.ByType, "SELECT t.transaction_type AS k, " + measures + " FROM transactions t WHERE t.booking_date BETWEEN ? AND ? GROUP BY t.transaction_type", args()},
		{&r.ByProvider, "SELECT t.provider AS k, " + measures + " FROM transactions t WHERE t.booking_date BETWEEN ? AND ? AND t.provider IS NOT NULL GROUP BY t.provider", args()},
		{&r.BySegment, "SELECT c.type AS k, " + measures + " FROM transactions t JOIN customers c ON c.id = COALESCE(t.ordering_customer_id, t.beneficiary_customer_id)" +
			" WHERE t.booking_date BETWEEN ? AND ? GROUP BY c.type", args()},
		{&r.Declines, "SELECT t.decline_code AS k, " + measures + " FROM transactions t WHERE t.booking_date BETWEEN ? AND ? AND t.status = ? AND t.decline_code IS NOT NULL" +
			" GROUP BY t.decline_code", args(domain.TxnFailed)},
	} {
		rows, err := store.Select[Breakdown](q, d.sql, d.args...)
		if err != nil {
			return nil, err
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
		*d.target = rows
	}
	if len(r.Declines) > 10 {
		r.Declines = r.Declines[:10]
	}

	if r.TopMerchants, err = store.Select[MerchantRank](q, `SELECT t.beneficiary_customer_id AS id, MAX(c.legal_name) AS name, COUNT(*) AS n, SUM(t.amount) AS v
	    FROM transactions t JOIN customers c ON c.id = t.beneficiary_customer_id
	    WHERE t.booking_date BETWEEN ? AND ? AND t.flow = ? AND t.status = ? AND t.currency = ? AND c.type = ?
	    GROUP BY t.beneficiary_customer_id ORDER BY v DESC LIMIT 10`,
		from.String(), to.String(), domain.FlowCollection, domain.TxnCompleted, currency, domain.Business); err != nil {
		return nil, err
	}

	if currencies, err := store.Select[grouped](q, "SELECT currency AS k, COUNT(*) AS n FROM transactions WHERE booking_date BETWEEN ? AND ? GROUP BY currency", period...); err != nil {
		return nil, err
	} else {
		r.Currencies = []string{}
		for _, c := range currencies {
			r.Currencies = append(r.Currencies, c.Key)
		}
		sort.Strings(r.Currencies)
	}

	if r.Customers.ByType, r.Customers.Total, err = groupCounts(q, "SELECT type AS k, COUNT(*) AS n FROM customers GROUP BY type"); err != nil {
		return nil, err
	}
	if r.Customers.ByKyc, _, err = groupCounts(q, "SELECT kyc_status AS k, COUNT(*) AS n FROM customers GROUP BY kyc_status"); err != nil {
		return nil, err
	}
	tiers, err := store.Select[struct {
		Tier  int `db:"tier"`
		Count int `db:"n"`
	}](q, "SELECT tier, COUNT(*) AS n FROM customers GROUP BY tier")
	if err != nil {
		return nil, err
	}
	r.Customers.ByTier = map[string]int{}
	for _, t := range tiers {
		r.Customers.ByTier["Tier "+strconv.Itoa(t.Tier)] = t.Count
	}
	if r.Customers.New, err = a.newCustomers(q, from, to, r.Granularity); err != nil {
		return nil, err
	}
	if r.Accounts.ByCurrency, _, err = groupCounts(q, "SELECT currency AS k, COUNT(*) AS n FROM wallets WHERE closed_at IS NULL GROUP BY currency"); err != nil {
		return nil, err
	}
	if r.Accounts.Sub, err = store.Scalar[int](q, "SELECT COUNT(*) FROM wallets WHERE closed_at IS NULL AND parent_wallet_id IS NOT NULL"); err != nil {
		return nil, err
	}
	if r.Accounts.MultiCurrency, err = store.Scalar[int](q, `SELECT COUNT(*) FROM (SELECT customer_id FROM wallets
	    WHERE closed_at IS NULL AND parent_wallet_id IS NULL GROUP BY customer_id HAVING COUNT(DISTINCT currency) > 1) m`); err != nil {
		return nil, err
	}
	return r, nil
}

func (a *App) kpis(q store.Q, from, to domain.Date, currency string) (AnalyticsKPIs, error) {
	var k AnalyticsKPIs
	period := []any{from.String(), to.String()}
	byStatus, total, err := groupCounts(q, "SELECT status AS k, COUNT(*) AS n FROM transactions WHERE booking_date BETWEEN ? AND ? GROUP BY status", period...)
	if err != nil {
		return k, err
	}
	k.Transactions, k.Completed, k.Failed = total, byStatus[string(domain.TxnCompleted)], byStatus[string(domain.TxnFailed)]
	if decided := k.Completed + k.Failed; decided > 0 {
		k.SuccessRate = float64(k.Completed) / float64(decided)
	}
	money, err := store.Select[struct {
		Count int   `db:"n"`
		Value int64 `db:"v"`
		Fees  int64 `db:"f"`
	}](q, "SELECT COUNT(*) AS n, COALESCE(SUM(amount), 0) AS v, COALESCE(SUM(charge_amount), 0) AS f FROM transactions WHERE booking_date BETWEEN ? AND ? AND status = ? AND currency = ?",
		append(period, domain.TxnCompleted, currency)...)
	if err != nil {
		return k, err
	}
	if len(money) == 1 {
		k.Value, k.Fees = money[0].Value, money[0].Fees
		if money[0].Count > 0 {
			k.AverageTicket = money[0].Value / int64(money[0].Count)
		}
	}
	if k.ActiveCustomers, err = store.Scalar[int](q, `SELECT COUNT(*) FROM (
	    SELECT ordering_customer_id AS c FROM transactions WHERE booking_date BETWEEN ? AND ? AND ordering_customer_id IS NOT NULL
	    UNION SELECT beneficiary_customer_id AS c FROM transactions WHERE booking_date BETWEEN ? AND ? AND beneficiary_customer_id IS NOT NULL) active`,
		append(period, period...)...); err != nil {
		return k, err
	}
	start, end := from.Time(), to.AddDays(1).Time()
	if k.NewCustomers, err = store.Scalar[int](q, "SELECT COUNT(*) FROM customers WHERE created_at >= ? AND created_at < ?", start, end); err != nil {
		return k, err
	}
	if k.NewAccounts, err = store.Scalar[int](q, "SELECT COUNT(*) FROM wallets WHERE created_at >= ? AND created_at < ?", start, end); err != nil {
		return k, err
	}
	return k, nil
}

// series buckets the range by booking day, or by the hour of input for a short range.
func (a *App) series(q store.Q, from, to domain.Date, currency, granularity string) ([]AnalyticsBucket, error) {
	type row struct {
		Key    string `db:"k"`
		Status string `db:"status"`
		Count  int    `db:"n"`
		Value  int64  `db:"v"`
		Fees   int64  `db:"f"`
	}
	add := func(b *AnalyticsBucket, r row) {
		switch domain.TransactionStatus(r.Status) {
		case domain.TxnCompleted:
			b.Completed += r.Count
			b.Value += r.Value
			b.Fees += r.Fees
		case domain.TxnFailed:
			b.Failed += r.Count
		default:
			b.Other += r.Count
		}
	}
	measures := "status, COUNT(*) AS n, COALESCE(SUM(CASE WHEN currency = ? THEN amount ELSE 0 END), 0) AS v, " +
		"COALESCE(SUM(CASE WHEN currency = ? THEN charge_amount ELSE 0 END), 0) AS f"
	var out []AnalyticsBucket
	if granularity == "hour" {
		end := to.AddDays(1).Time()
		if now := a.now().UTC().Truncate(time.Hour).Add(time.Hour); now.Before(end) {
			end = now // no empty hours from the future
		}
		for h := from.Time(); h.Before(end); h = h.Add(time.Hour) {
			rows, err := store.Select[row](q, "SELECT '' AS k, "+measures+" FROM transactions WHERE created_at >= ? AND created_at < ? GROUP BY status",
				currency, currency, h, h.Add(time.Hour))
			if err != nil {
				return nil, err
			}
			b := AnalyticsBucket{Bucket: h.Format(time.RFC3339)}
			for _, r := range rows {
				add(&b, r)
			}
			out = append(out, b)
		}
		return out, nil
	}
	rows, err := store.Select[row](q, "SELECT booking_date AS k, "+measures+" FROM transactions WHERE booking_date BETWEEN ? AND ? GROUP BY booking_date, status",
		currency, currency, from.String(), to.String())
	if err != nil {
		return nil, err
	}
	byDay := map[string]*AnalyticsBucket{}
	for d := from; !d.After(to); d = d.AddDays(1) {
		out = append(out, AnalyticsBucket{Bucket: d.String()})
	}
	for i := range out {
		byDay[out[i].Bucket] = &out[i]
	}
	for _, r := range rows {
		if b := byDay[r.Key]; b != nil {
			add(b, r)
		}
	}
	return out, nil
}

// newCustomers counts customers created per bucket. Creation times are bucketed here rather than
// in SQL, whose date functions differ by engine.
func (a *App) newCustomers(q store.Q, from, to domain.Date, granularity string) ([]AnalyticsCount, error) {
	created, err := store.Select[struct {
		At time.Time `db:"created_at"`
	}](q, "SELECT created_at FROM customers WHERE created_at >= ? AND created_at < ?", from.Time(), to.AddDays(1).Time())
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, c := range created {
		if granularity == "hour" {
			counts[c.At.UTC().Truncate(time.Hour).Format(time.RFC3339)]++
		} else {
			counts[domain.DateOf(c.At.UTC()).String()]++
		}
	}
	var out []AnalyticsCount
	if granularity == "hour" {
		end := to.AddDays(1).Time()
		if now := a.now().UTC().Truncate(time.Hour).Add(time.Hour); now.Before(end) {
			end = now
		}
		for h := from.Time(); h.Before(end); h = h.Add(time.Hour) {
			key := h.Format(time.RFC3339)
			out = append(out, AnalyticsCount{key, counts[key]})
		}
		return out, nil
	}
	for d := from; !d.After(to); d = d.AddDays(1) {
		out = append(out, AnalyticsCount{d.String(), counts[d.String()]})
	}
	return out, nil
}
