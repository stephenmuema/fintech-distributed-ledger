package app

import (
	"context"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Dashboard is the console's front page: who banks with us, what moved in the last 24 hours,
// and what waits for a person. Every figure is a count or a sum over indexed columns.
type Dashboard struct {
	Customers struct {
		Total  int            `json:"total"`
		ByType map[string]int `json:"byType"`
		New24h int            `json:"new24h"`
	} `json:"customers"`
	Accounts struct {
		Open       int            `json:"open"`
		Sub        int            `json:"sub"`
		Closed     int            `json:"closed"`
		ByCurrency map[string]int `json:"byCurrency"`
	} `json:"accounts"`
	Transactions struct {
		Last24h  int              `json:"last24h"`
		ByStatus map[string]int   `json:"byStatus"`
		ByFlow   map[string]int   `json:"byFlow"`
		Value24h map[string]int64 `json:"value24h"` // completed, minor units per currency
		Hourly   []HourVolume     `json:"hourly"`
	} `json:"transactions"`
	Pending struct {
		Transactions  int `json:"transactions"`
		Changes       int `json:"changes"`
		PayoutReviews int `json:"payoutReviews"`
		// EscalatedPayouts: payouts no provider confirmed, waiting for operations to settle them.
		EscalatedPayouts int `json:"escalatedPayouts"`
	} `json:"pending"`
	AsOf time.Time `json:"asOf"`
}

// HourVolume is one hour's transactions by outcome.
type HourVolume struct {
	Hour      time.Time `json:"hour"`
	Completed int       `json:"completed"`
	Failed    int       `json:"failed"`
	Other     int       `json:"other"` // pending, processing, awaiting authorisation
}

type grouped struct {
	Key   string `db:"k"`
	Count int    `db:"n"`
}

func groupCounts(q store.Q, query string, args ...any) (map[string]int, int, error) {
	rows, err := store.Select[grouped](q, query, args...)
	out, total := map[string]int{}, 0
	for _, r := range rows {
		out[r.Key] += r.Count
		total += r.Count
	}
	return out, total, err
}

func (a *App) Dashboard(ctx context.Context) (*Dashboard, error) {
	q := a.read(ctx)
	now := a.now().UTC()
	since := now.Add(-24 * time.Hour)
	d := &Dashboard{AsOf: now}
	var err error
	if d.Customers.ByType, d.Customers.Total, err = groupCounts(q, "SELECT type AS k, COUNT(*) AS n FROM customers GROUP BY type"); err != nil {
		return nil, err
	}
	if d.Customers.New24h, err = store.Scalar[int](q, "SELECT COUNT(*) FROM customers WHERE created_at >= ?", since); err != nil {
		return nil, err
	}
	if d.Accounts.ByCurrency, d.Accounts.Open, err = groupCounts(q, "SELECT currency AS k, COUNT(*) AS n FROM wallets WHERE closed_at IS NULL GROUP BY currency"); err != nil {
		return nil, err
	}
	if d.Accounts.Sub, err = store.Scalar[int](q, "SELECT COUNT(*) FROM wallets WHERE closed_at IS NULL AND parent_wallet_id IS NOT NULL"); err != nil {
		return nil, err
	}
	if d.Accounts.Closed, err = store.Scalar[int](q, "SELECT COUNT(*) FROM wallets WHERE closed_at IS NOT NULL"); err != nil {
		return nil, err
	}
	if d.Transactions.ByStatus, d.Transactions.Last24h, err = groupCounts(q, "SELECT status AS k, COUNT(*) AS n FROM transactions WHERE created_at >= ? GROUP BY status", since); err != nil {
		return nil, err
	}
	if d.Transactions.ByFlow, _, err = groupCounts(q, "SELECT flow AS k, COUNT(*) AS n FROM transactions WHERE created_at >= ? GROUP BY flow", since); err != nil {
		return nil, err
	}
	values, err := store.Select[struct {
		Currency string `db:"currency"`
		Total    int64  `db:"total"`
	}](q, "SELECT currency, COALESCE(SUM(amount), 0) AS total FROM transactions WHERE created_at >= ? AND status = ? GROUP BY currency", since, domain.TxnCompleted)
	if err != nil {
		return nil, err
	}
	d.Transactions.Value24h = map[string]int64{}
	for _, v := range values {
		d.Transactions.Value24h[v.Currency] = v.Total
	}
	start := now.Truncate(time.Hour).Add(-23 * time.Hour)
	for h := range 24 {
		from := start.Add(time.Duration(h) * time.Hour)
		byStatus, _, err := groupCounts(q, "SELECT status AS k, COUNT(*) AS n FROM transactions WHERE created_at >= ? AND created_at < ? GROUP BY status", from, from.Add(time.Hour))
		if err != nil {
			return nil, err
		}
		v := HourVolume{Hour: from, Completed: byStatus[string(domain.TxnCompleted)], Failed: byStatus[string(domain.TxnFailed)]}
		for status, n := range byStatus {
			if status != string(domain.TxnCompleted) && status != string(domain.TxnFailed) {
				v.Other += n
			}
		}
		d.Transactions.Hourly = append(d.Transactions.Hourly, v)
	}
	if d.Pending.Transactions, err = store.Scalar[int](q, "SELECT COUNT(*) FROM transactions WHERE record_status = ?", domain.RecordUnauthorised); err != nil {
		return nil, err
	}
	if d.Pending.Changes, err = store.Scalar[int](q, "SELECT COUNT(*) FROM parameter_change_requests WHERE status = ?", domain.Unauthorised); err != nil {
		return nil, err
	}
	if d.Pending.PayoutReviews, err = store.Scalar[int](q, "SELECT COUNT(*) FROM payouts WHERE status = ?", domain.PayoutPendingReview); err != nil {
		return nil, err
	}
	if d.Pending.EscalatedPayouts, err = store.Scalar[int](q, "SELECT COUNT(*) FROM payouts WHERE status = ? AND escalated_at IS NOT NULL", domain.PayoutProcessing); err != nil {
		return nil, err
	}
	return d, nil
}
