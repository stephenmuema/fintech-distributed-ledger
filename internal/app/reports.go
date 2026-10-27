package app

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Reports is the RE module — general-ledger reporting. Every ledger account consolidates into a
// GL line via its category; balances are reported per currency and in local currency (LCY) at
// mid-rate. Reports are live, or as of a closed business date from COB balance snapshots.
type Reports struct{ a *App }

type AccountDrift struct {
	AccountID      uuid.UUID `json:"accountId"`
	AccountNumber  string    `json:"accountNumber"`
	StoredBalance  int64     `json:"storedBalance"`
	DerivedBalance int64     `json:"derivedBalance"`
}

type GLLine struct {
	GLCode     string             `json:"glCode"`
	Name       string             `json:"name"`
	Type       domain.AccountType `json:"type"`
	Section    domain.GLSection   `json:"section"`
	Group      string             `json:"group"`
	Currency   string             `json:"currency"`
	Balance    int64              `json:"balance"`
	BalanceLCY decimal.Decimal    `json:"balanceLcy"`
}

type GLStatement struct {
	AsOf          *domain.Date `json:"asOf"`
	LocalCurrency string       `json:"localCurrency"`
	Lines         []GLLine     `json:"lines"`
}

type BalanceSheet struct {
	AsOf                *domain.Date    `json:"asOf"`
	LocalCurrency       string          `json:"localCurrency"`
	Assets              decimal.Decimal `json:"assets"`
	Liabilities         decimal.Decimal `json:"liabilities"`
	Equity              decimal.Decimal `json:"equity"`
	CurrentYearEarnings decimal.Decimal `json:"currentYearEarnings"`
	Balances            bool            `json:"balances"`
	Lines               []GLLine        `json:"lines"`
}

type ProfitAndLoss struct {
	AsOf          *domain.Date    `json:"asOf"`
	LocalCurrency string          `json:"localCurrency"`
	Income        decimal.Decimal `json:"income"`
	Expenses      decimal.Decimal `json:"expenses"`
	NetProfit     decimal.Decimal `json:"netProfit"`
	Lines         []GLLine        `json:"lines"`
}

type TrialBalanceLine struct {
	Currency          string `json:"currency"`
	DebitNormalTotal  int64  `json:"debitNormalTotal"`
	CreditNormalTotal int64  `json:"creditNormalTotal"`
	Balanced          bool   `json:"balanced"`
}

type TransactionLeg struct {
	AccountNumber string                `json:"accountNumber"`
	AccountName   string                `json:"accountName"`
	GLCode        string                `json:"glCode"`
	Direction     domain.EntryDirection `json:"direction"`
	Amount        int64                 `json:"amount"`
	Currency      string                `json:"currency"`
	BalanceAfter  int64                 `json:"balanceAfter"`
}

type TransactionView struct {
	ID                   uuid.UUID            `json:"id"`
	TransactionReference string               `json:"transactionReference"`
	Reference            string               `json:"reference"`
	Category             domain.EntryCategory `json:"category"`
	Description          string               `json:"description"`
	BookingDate          domain.Date          `json:"bookingDate"`
	ValueDate            domain.Date          `json:"valueDate"`
	PostedAt             time.Time            `json:"postedAt"`
	Legs                 []TransactionLeg     `json:"legs"`
}

type accountTotals struct {
	Type     domain.AccountType `db:"type"`
	Currency string             `db:"currency"`
	Balance  int64              `db:"balance"`
}

func (r *Reports) TrialBalance(ctx context.Context) ([]TrialBalanceLine, error) {
	rows, err := store.Select[accountTotals](r.a.read(ctx), "SELECT type, currency, SUM(balance) AS balance FROM ledger_accounts GROUP BY type, currency")
	if err != nil {
		return nil, err
	}
	byCurrency := map[string]*TrialBalanceLine{}
	for _, row := range rows {
		line := byCurrency[row.Currency]
		if line == nil {
			line = &TrialBalanceLine{Currency: row.Currency}
			byCurrency[row.Currency] = line
		}
		if row.Type.IsDebitNormal() {
			line.DebitNormalTotal += row.Balance
		} else {
			line.CreditNormalTotal += row.Balance
		}
	}
	lines := []TrialBalanceLine{}
	for _, l := range byCurrency {
		l.Balanced = l.DebitNormalTotal == l.CreditNormalTotal
		lines = append(lines, *l)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].Currency < lines[j].Currency })
	return lines, nil
}

// Integrity recomputes every account balance from its journal lines and reports any drift.
func (r *Reports) Integrity(ctx context.Context) ([]AccountDrift, error) {
	rows, err := store.Select[struct {
		ID            uuid.UUID          `db:"id"`
		AccountNumber string             `db:"account_number"`
		Type          domain.AccountType `db:"type"`
		Balance       int64              `db:"balance"`
		Net           int64              `db:"net"`
	}](r.a.read(ctx), `SELECT a.id, a.account_number, a.type, a.balance, COALESCE(d.net, 0) AS net
	    FROM ledger_accounts a
	    LEFT JOIN (SELECT account_id, SUM(CASE direction WHEN 'Debit' THEN amount ELSE -amount END) AS net
	               FROM journal_lines GROUP BY account_id) d ON d.account_id = a.id`)
	if err != nil {
		return nil, err
	}
	drift := []AccountDrift{}
	for _, row := range rows {
		derived := row.Net
		if !row.Type.IsDebitNormal() {
			derived = -derived
		}
		if derived != row.Balance {
			drift = append(drift, AccountDrift{row.ID, row.AccountNumber, row.Balance, derived})
		}
	}
	return drift, nil
}

// GLBalances returns balances by GL line and currency (T24: CRF / GL report lines).
func (r *Reports) GLBalances(ctx context.Context, asOf *domain.Date) (*GLStatement, error) {
	q := r.a.read(ctx)
	type row struct {
		GLCode   string `db:"gl_code"`
		Currency string `db:"currency"`
		Balance  int64  `db:"balance"`
	}
	var rows []row
	var err error
	if asOf == nil {
		rows, err = store.Select[row](q, "SELECT gl_code, currency, SUM(balance) AS balance FROM ledger_accounts GROUP BY gl_code, currency")
	} else {
		exists, xerr := store.Exists(q, "SELECT 1 FROM daily_balances WHERE business_date = ?", *asOf)
		if xerr != nil {
			return nil, xerr
		}
		if !exists {
			return nil, domain.Rule("no_snapshot", "No COB balance snapshot exists for %s.", asOf)
		}
		rows, err = store.Select[row](q, `SELECT a.gl_code, b.currency, SUM(b.closing) AS balance
		    FROM daily_balances b JOIN ledger_accounts a ON a.id = b.account_id
		    WHERE b.business_date = ? GROUP BY a.gl_code, b.currency`, *asOf)
	}
	if err != nil {
		return nil, err
	}

	lines := []GLLine{}
	for _, row := range rows {
		if row.Balance == 0 {
			continue
		}
		gl, _ := domain.GLAccountByCode(row.GLCode)
		lcy, err := r.toLCY(row.Balance, row.Currency)
		if err != nil {
			return nil, err
		}
		lines = append(lines, GLLine{gl.Code, gl.Name, gl.Type, gl.Section, gl.Group, row.Currency, row.Balance, lcy})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].GLCode != lines[j].GLCode {
			return lines[i].GLCode < lines[j].GLCode
		}
		return lines[i].Currency < lines[j].Currency
	})
	return &GLStatement{asOf, r.a.Options.LocalCurrency, lines}, nil
}

func (r *Reports) BalanceSheet(ctx context.Context, asOf *domain.Date) (*BalanceSheet, error) {
	gl, err := r.GLBalances(ctx, asOf)
	if err != nil {
		return nil, err
	}
	sum := sumByType(gl.Lines)
	assets, liabilities, equity := sum[domain.Asset], sum[domain.Liability], sum[domain.Equity]
	earnings := sum[domain.Revenue].Sub(sum[domain.Expense])
	lines := []GLLine{}
	for _, l := range gl.Lines {
		if l.Section == domain.BalanceSheetSection {
			lines = append(lines, l)
		}
	}
	// Each foreign-currency line is translated to the local currency and rounded on its own, so
	// the sides may differ by up to half a minor unit per translated line even when every
	// currency balances exactly (the trial balance checks that, in each currency).
	tolerance := decimal.RequireFromString("0.01")
	for _, l := range gl.Lines {
		if l.Currency != gl.LocalCurrency {
			tolerance = tolerance.Add(decimal.RequireFromString("0.005"))
		}
	}
	balances := assets.Sub(liabilities.Add(equity).Add(earnings)).Abs().LessThan(tolerance)
	return &BalanceSheet{asOf, gl.LocalCurrency, assets, liabilities, equity, earnings, balances, lines}, nil
}

func (r *Reports) ProfitAndLoss(ctx context.Context, asOf *domain.Date) (*ProfitAndLoss, error) {
	gl, err := r.GLBalances(ctx, asOf)
	if err != nil {
		return nil, err
	}
	lines := []GLLine{}
	for _, l := range gl.Lines {
		if l.Section == domain.ProfitAndLossSection {
			lines = append(lines, l)
		}
	}
	sum := sumByType(lines)
	income, expenses := sum[domain.Revenue], sum[domain.Expense]
	return &ProfitAndLoss{asOf, gl.LocalCurrency, income, expenses, income.Sub(expenses), lines}, nil
}

func sumByType(lines []GLLine) map[domain.AccountType]decimal.Decimal {
	sum := map[domain.AccountType]decimal.Decimal{}
	for _, l := range lines {
		sum[l.Type] = sum[l.Type].Add(l.BalanceLCY)
	}
	return sum
}

// InternalAccounts lists the internal and P&L accounts, each split account once with its total.
func (r *Reports) InternalAccounts(ctx context.Context) ([]domain.LedgerAccount, error) {
	q := r.a.read(ctx)
	accounts, err := store.Select[domain.LedgerAccount](q, accountColumns+" WHERE category >= 10000 ORDER BY category, currency, account_number")
	if err != nil {
		return nil, err
	}
	return r.a.Ledger.Totals(q, accounts)
}

// Entry is the journal entry enquiry by its reference: the entry and all of its legs.
func (r *Reports) Entry(ctx context.Context, reference string) (*TransactionView, error) {
	q := r.a.read(ctx)
	entries, err := store.Select[domain.JournalEntry](q, "SELECT * FROM journal_entries WHERE transaction_reference = ?", reference)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, domain.NotFound("journal_entry", reference)
	}
	views, err := r.views(q, entries)
	if err != nil {
		return nil, err
	}
	return &views[0], nil
}

// Journal is the transaction journal for a booking date (T24: TXN.JOURNAL enquiry).
func (r *Reports) Journal(ctx context.Context, bookingDate domain.Date, category *domain.EntryCategory, limit int) ([]TransactionView, error) {
	q := r.a.read(ctx)
	query, args := "SELECT * FROM journal_entries WHERE booking_date = ?", []any{bookingDate}
	if category != nil {
		query += " AND category = ?"
		args = append(args, *category)
	}
	entries, err := store.Select[domain.JournalEntry](q, query+" ORDER BY posted_at LIMIT ?", append(args, clamp(limit, 1, 500))...)
	if err != nil {
		return nil, err
	}
	return r.views(q, entries)
}

func (r *Reports) views(q store.Q, entries []domain.JournalEntry) ([]TransactionView, error) {
	views := make([]TransactionView, 0, len(entries))
	if len(entries) == 0 {
		return views, nil
	}
	ids := make([]uuid.UUID, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	marks, args := store.In(ids)
	legs, err := store.Select[struct {
		JournalEntryID uuid.UUID             `db:"journal_entry_id"`
		AccountNumber  string                `db:"account_number"`
		AccountName    string                `db:"name"`
		GLCode         string                `db:"gl_code"`
		Direction      domain.EntryDirection `db:"direction"`
		Amount         int64                 `db:"amount"`
		Currency       string                `db:"currency"`
		BalanceAfter   int64                 `db:"balance_after"`
	}](q, `SELECT l.journal_entry_id, a.account_number, a.name, a.gl_code, l.direction, l.amount, l.currency, l.balance_after
	       FROM journal_lines l JOIN ledger_accounts a ON a.id = l.account_id
	       WHERE l.journal_entry_id IN (`+marks+`) ORDER BY l.id`, args...)
	if err != nil {
		return nil, err
	}
	byEntry := map[uuid.UUID][]TransactionLeg{}
	for _, l := range legs {
		byEntry[l.JournalEntryID] = append(byEntry[l.JournalEntryID], TransactionLeg{l.AccountNumber, l.AccountName, l.GLCode, l.Direction, l.Amount, l.Currency, l.BalanceAfter})
	}
	for _, e := range entries {
		views = append(views, TransactionView{e.ID, e.TransactionReference, e.Reference, e.Category, e.Description, e.BookingDate, e.ValueDate, e.PostedAt, byEntry[e.ID]})
	}
	return views, nil
}

// toLCY converts minor units to local-currency major units at mid-rate, rounded to 2 dp (banker's).
func (r *Reports) toLCY(minor int64, currency string) (decimal.Decimal, error) {
	ccy, err := domain.CurrencyOf(currency)
	if err != nil {
		return decimal.Zero, err
	}
	major := ccy.ToMajor(minor)
	if currency != r.a.Options.LocalCurrency {
		rate, err := r.a.Rates.MidRate(currency, r.a.Options.LocalCurrency)
		if err != nil {
			return decimal.Zero, err
		}
		major = major.Mul(rate)
	}
	return major.RoundBank(2), nil
}
