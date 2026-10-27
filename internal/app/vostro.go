package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/statements"
	"github.com/stephenmuema/finnie/internal/store"
)

// Vostro (NV) is the other side of correspondent banking: accounts other banks and PSPs hold
// with us. A vostro is a customer account of a FinancialInstitution customer (category 5001,
// GL 2120 "due to banks"), so the posting restrictions, no-overdraft rule, interest and
// statements of customer accounts all apply. What the holder needs on top is a statement in
// the format its own reconciliation reads: MT940, or CSV.
type Vostro struct{ a *App }

// VostroView is a vostro account with its holder.
type VostroView struct {
	AccountView
	HolderName     string `json:"holderName"`
	CustomerNumber int64  `json:"customerNumber"`
	Correspondent  string `json:"correspondent"`
}

func (v *Vostro) Accounts(ctx context.Context) ([]VostroView, error) {
	q := v.a.read(ctx)
	wallets, err := store.Select[domain.Wallet](q, "SELECT * FROM wallets WHERE category = ? ORDER BY account_number", domain.CategoryVostro)
	if err != nil {
		return nil, err
	}
	views, err := v.a.Accounts.views(q, wallets)
	if err != nil {
		return nil, err
	}
	out := []VostroView{}
	for _, view := range views {
		customer, err := store.Get[domain.Customer](q, "SELECT * FROM customers WHERE id = ?", view.Wallet.CustomerID)
		if err != nil {
			return nil, err
		}
		row := VostroView{AccountView: view}
		if customer != nil {
			row.HolderName, row.CustomerNumber = customer.LegalName, customer.CustomerNumber
		}
		c, err := store.Get[domain.Correspondent](q, "SELECT * FROM correspondents WHERE customer_id = ?", view.Wallet.CustomerID)
		if err != nil {
			return nil, err
		}
		if c != nil {
			row.Correspondent = c.Code
		}
		out = append(out, row)
	}
	return out, nil
}

// StatementFile is a rendered statement.
type StatementFile struct {
	FileName    string
	ContentType string
	Body        []byte
}

// Export renders an account's statement for [from, to] as MT940 or CSV. Directions are the
// holder's: a credit is money into their account with us.
func (v *Vostro) Export(ctx context.Context, walletID uuid.UUID, from, to domain.Date, format string) (*StatementFile, error) {
	if to.Before(from) {
		return nil, domain.Rule("invalid_period", "The statement period ends before it starts.")
	}
	if from.DaysUntil(to) > 366 {
		return nil, domain.Rule("invalid_period", "A statement covers at most a year.")
	}
	q := v.a.read(ctx)
	w, err := v.a.Accounts.loadWallet(q, walletID)
	if err != nil {
		return nil, err
	}
	rows, err := store.Select[struct {
		TransactionReference string                `db:"transaction_reference"`
		Description          string                `db:"description"`
		Direction            domain.EntryDirection `db:"direction"`
		Amount               int64                 `db:"amount"`
		BalanceAfter         int64                 `db:"balance_after"`
		BookingDate          domain.Date           `db:"booking_date"`
		ValueDate            domain.Date           `db:"value_date"`
	}](q, `SELECT e.transaction_reference, e.description, l.direction, l.amount, l.balance_after, l.booking_date, e.value_date
	       FROM journal_lines l JOIN journal_entries e ON e.id = l.journal_entry_id
	       WHERE l.account_id = ? AND l.booking_date >= ? AND l.booking_date <= ?
	       ORDER BY e.posted_at, l.id LIMIT 10000`, w.AvailableAccountID, from, to)
	if err != nil {
		return nil, err
	}
	opening, err := store.Scalar[int64](q, `SELECT COALESCE(SUM(CASE direction WHEN 'Credit' THEN amount ELSE -amount END), 0)
	       FROM journal_lines WHERE account_id = ? AND booking_date < ?`, w.AvailableAccountID, from)
	if err != nil {
		return nil, err
	}
	closing := opening
	lines := make([]statements.ExportLine, len(rows))
	for i, r := range rows {
		direction := "Credit"
		if r.Direction == domain.Debit {
			direction = "Debit"
			closing -= r.Amount
		} else {
			closing += r.Amount
		}
		lines[i] = statements.ExportLine{ValueDate: r.ValueDate, BookingDate: r.BookingDate, Direction: direction, Amount: r.Amount,
			Reference: r.TransactionReference, Narrative: r.Description}
	}
	base := fmt.Sprintf("%s_%s_%s", w.AccountNumber, from.Format("20060102"), to.Format("20060102"))
	switch format {
	case "", "mt940":
		body, err := statements.WriteMT940("ST"+to.Format("060102")+w.AccountNumber[len(w.AccountNumber)-4:], w.AccountNumber, to.Format("0102")+"/1", w.Currency,
			statements.Balance{Date: from.AddDays(-1), Amount: opening}, statements.Balance{Date: to, Amount: closing}, lines)
		if err != nil {
			return nil, err
		}
		return &StatementFile{FileName: base + ".mt940", ContentType: "text/plain; charset=utf-8", Body: []byte(body)}, nil
	case "csv":
		var buf bytes.Buffer
		cw := csv.NewWriter(&buf)
		_ = cw.Write([]string{"value_date", "booking_date", "direction", "amount", "reference", "narrative", "currency"})
		_ = cw.Write([]string{from.AddDays(-1).String(), "", "Opening", strconv.FormatInt(opening, 10), "", "Opening balance", w.Currency})
		for _, l := range lines {
			_ = cw.Write([]string{l.ValueDate.String(), l.BookingDate.String(), l.Direction, strconv.FormatInt(l.Amount, 10), l.Reference, l.Narrative, w.Currency})
		}
		_ = cw.Write([]string{to.String(), "", "Closing", strconv.FormatInt(closing, 10), "", "Closing balance", w.Currency})
		cw.Flush()
		return &StatementFile{FileName: base + ".csv", ContentType: "text/csv; charset=utf-8", Body: buf.Bytes()}, cw.Error()
	}
	return nil, domain.Rule("invalid_format", "Format must be mt940 or csv.")
}
