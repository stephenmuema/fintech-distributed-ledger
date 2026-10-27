package app

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Ledger (EB.ACCOUNTING) is the only way money moves in Zirnis. Every feature — collections,
// payouts, FX, cards, disputes, settlement, COB interest — is a balanced journal entry posted here.
type Ledger struct {
	a     *App
	parts sync.Map // part code → id
}

const accountColumns = "SELECT * FROM ledger_accounts"

func (l *Ledger) load(tx *store.Tx, where string, arg any, missing func() error) (*domain.LedgerAccount, error) {
	acct, err := store.Get[domain.LedgerAccount](tx, accountColumns+" WHERE "+where, arg)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return nil, missing()
	}
	return tx.TrackAccount(acct), nil
}

// SystemAccount resolves a platform account by its deterministic code.
func (l *Ledger) SystemAccount(tx *store.Tx, spec domain.SystemAccountSpec) (*domain.LedgerAccount, error) {
	return l.load(tx, "code = ?", spec.Code, func() error {
		return fmt.Errorf("system account '%s' is missing: bootstrap the ledger first", spec.Code)
	})
}

// Account loads a ledger account by id (once per unit of work).
func (l *Ledger) Account(tx *store.Tx, id uuid.UUID) (*domain.LedgerAccount, error) {
	if a := tx.CachedAccount(id); a != nil {
		return a, nil
	}
	return l.load(tx, "id = ?", id, func() error { return domain.NotFound("ledger_account", id) })
}

func (l *Ledger) AccountByNumber(tx *store.Tx, number string) (*domain.LedgerAccount, error) {
	return l.load(tx, "account_number = ?", number, func() error { return domain.NotFound("account", number) })
}

// WalletAccounts returns a wallet's available and held ledger accounts.
func (l *Ledger) WalletAccounts(tx *store.Tx, w *domain.Wallet) (available, held *domain.LedgerAccount, err error) {
	if available, err = l.Account(tx, w.AvailableAccountID); err != nil {
		return nil, nil, err
	}
	held, err = l.Account(tx, w.HeldAccountID)
	return available, held, err
}

// PostOptions tune a posting.
type PostOptions struct {
	// BookingDate overrides the online booking date (COB jobs book to the day being closed).
	BookingDate *domain.Date
	// OverdraftPermitted lists accounts allowed to go negative for this posting only.
	OverdraftPermitted []*domain.LedgerAccount
}

// Post validates the entry and writes it — entry, lines and new balances — in the caller's
// unit of work, so a payment can never be marked succeeded without its money movement, or
// vice versa.
func (l *Ledger) Post(tx *store.Tx, b *domain.EntryBuilder, opts ...PostOptions) (*domain.JournalEntry, error) {
	var o PostOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	duplicate, err := store.Exists(tx, "SELECT 1 FROM journal_entries WHERE reference = ?", b.Reference)
	if err != nil {
		return nil, err
	}
	if duplicate {
		return nil, domain.Conflict("duplicate_posting", "Journal entry '%s' was already posted.", b.Reference)
	}

	date := o.BookingDate
	if date == nil {
		d, err := l.a.Dates.OnlineBookingDate(tx)
		if err != nil {
			return nil, err
		}
		date = &d
	}

	// A per-day, per-prefix counter makes the T24 reference collision-free at any volume (0, 1, 2…).
	sequence, err := l.a.nextReferenceSequence(tx, *date, domain.TransactionReferencePrefix(b.Category()))
	if err != nil {
		return nil, err
	}
	if err := l.spread(tx, b, &o); err != nil {
		return nil, err
	}
	direct, shared := l.split(tx, b)
	// A shared account's balance is checked when it's written, at commit (ledgerFlush); its
	// in-memory balance here may be behind what other units of work have committed since.
	entry, err := b.Sequence(sequence).Post(tx.Now(), *date, append(o.OverdraftPermitted, shared...)...)
	if err != nil {
		return nil, err
	}
	entry.EntryHash = entry.ComputeHash()
	if entry.TransactionID, err = l.a.Transactions.entryTransaction(tx, b.Reference); err != nil {
		return nil, err
	}
	if err := tx.Insert("journal_entries", entry); err != nil {
		if store.IsUniqueViolation(err) { // posted meanwhile by a concurrent unit of work
			return nil, domain.Conflict("duplicate_posting", "Journal entry '%s' was already posted.", b.Reference)
		}
		return nil, err
	}
	flush := l.flushFor(tx)
	legs := make([]map[string]any, len(entry.Lines))
	for i := range entry.Lines {
		line := &entry.Lines[i]
		legs[i] = map[string]any{"accountId": line.AccountID, "direction": line.Direction, "amount": line.Amount, "currency": line.Currency}
		if flush != nil && flush.defer_(line, shared, o.OverdraftPermitted) {
			continue // written with its running balance at commit
		}
		if err := tx.Insert("journal_lines", line); err != nil {
			return nil, err
		}
	}
	for _, acct := range direct {
		if err := tx.Update("ledger_accounts", acct); err != nil {
			return nil, err
		}
		if !tx.Serial() {
			// The flow checked the account was open before posting; with parallel writers it may
			// have closed since. Now that this unit of work holds the account's row, look again:
			// a close either committed before (seen here) or waits for this posting and then
			// finds the money (Accounts.fence).
			closed, err := store.Exists(tx, "SELECT 1 FROM wallets WHERE available_account_id = ? AND closed_at IS NOT NULL "+
				"UNION ALL SELECT 1 FROM wallets WHERE held_account_id = ? AND closed_at IS NOT NULL", acct.ID, acct.ID)
			if err != nil {
				return nil, err
			}
			if closed {
				return nil, domain.Conflict("account_closed", "Account %s is closed.", acct.AccountNumber)
			}
		}
	}
	// Seal the posting into the hash chain: the chain then vouches for every amount in the ledger.
	if err := tx.Audit("ledger.posted", "journal_entry", entry.ID.String(), map[string]any{
		"reference": entry.Reference, "transactionReference": entry.TransactionReference, "category": entry.Category,
		"bookingDate": entry.BookingDate, "entryHash": entry.EntryHash, "legs": legs,
	}); err != nil {
		return nil, err
	}
	tx.AfterCommit(func() { l.a.Metrics.LedgerPostings.Inc("category", string(entry.Category)) })
	return entry, nil
}

// spread moves the legs on a split account to the part this unit of work posts to (ADR 0028).
// A unit of work uses one part throughout, so a book it credits and debits in the same commit (a
// collection passing through the provider's inward book) nets to zero in that part.
func (l *Ledger) spread(tx *store.Tx, b *domain.EntryBuilder, o *PostOptions) error {
	if tx.Serial() || l.a.Options.LedgerParts <= 1 {
		return nil
	}
	for _, a := range b.Accounts() {
		if !a.Splits() {
			continue
		}
		part, err := l.part(tx, a)
		if err != nil {
			return err
		}
		b.Replace(a, part)
		if slices.Contains(o.OverdraftPermitted, a) {
			o.OverdraftPermitted = append(o.OverdraftPermitted, part)
		}
	}
	return nil
}

const ledgerPartKey = "ledger.part"

// part returns the part of a split account this unit of work posts to, picked at random once
// per unit of work; part 0 is the account itself.
func (l *Ledger) part(tx *store.Tx, a *domain.LedgerAccount) (*domain.LedgerAccount, error) {
	n, ok := tx.Value(ledgerPartKey).(int)
	if !ok {
		n = int(domain.RandomInt(int64(l.a.Options.LedgerParts)))
		tx.SetValue(ledgerPartKey, n)
	}
	if n == 0 {
		return a, nil
	}
	code := domain.PartCode(a.Code, n)
	id, ok := l.parts.Load(code)
	if !ok {
		opened, err := l.openPart(tx, a, n)
		if err != nil {
			return nil, err
		}
		id, _ = l.parts.LoadOrStore(code, opened)
	}
	return l.Account(tx, id.(uuid.UUID))
}

// openPart finds or opens part n of a split account in its own short transaction, so the part
// exists whether or not this unit of work commits, and two instances opening it agree.
func (l *Ledger) openPart(tx *store.Tx, a *domain.LedgerAccount, n int) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.Store().Independent(tx.Context(), func(q store.Q) error {
		existing, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE code = ?", domain.PartCode(a.Code, n))
		if err != nil || existing != nil {
			if existing != nil {
				id = existing.ID
			}
			return err
		}
		part := a.NewPart(n, tx.Now())
		id = part.ID
		return store.Insert(q, "ledger_accounts", part) // opened meanwhile elsewhere: a unique violation, then found
	})
	return id, err
}

// Totals is accounts as views show them: each split account with its total over its parts, and
// the parts left out (ADR 0028).
func (l *Ledger) Totals(q store.Q, accounts []domain.LedgerAccount) ([]domain.LedgerAccount, error) {
	var parents []uuid.UUID
	for _, a := range accounts {
		if a.Splits() {
			parents = append(parents, a.ID)
		}
	}
	extra := map[uuid.UUID]int64{}
	if len(parents) > 0 {
		marks, args := store.In(parents)
		rows, err := store.Select[struct {
			ParentID uuid.UUID `db:"parent_id"`
			Balance  int64     `db:"balance"`
		}](q, "SELECT parent_id, SUM(balance) AS balance FROM ledger_accounts WHERE parent_id IN ("+marks+") GROUP BY parent_id", args...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			extra[r.ParentID] = r.Balance
		}
	}
	out := make([]domain.LedgerAccount, 0, len(accounts))
	for _, a := range accounts {
		if a.ParentID != nil {
			continue
		}
		a.Balance += extra[a.ID]
		out = append(out, a)
	}
	return out, nil
}

// split separates a posting's accounts into those written now, with the version check, and those
// written at commit. On a serial store every account is written now, as before. With parallel
// writers, only customer accounts are: the shared ones (system books, fee revenue, provider
// floats), which every unit of work would contend for, are written by ledgerFlush just before
// commit, in one global order, so writers neither retry on them nor deadlock over them.
func (l *Ledger) split(tx *store.Tx, b *domain.EntryBuilder) (direct, shared []*domain.LedgerAccount) {
	accounts := b.Accounts()
	if tx.Serial() {
		return accounts, nil
	}
	slices.SortFunc(accounts, func(x, y *domain.LedgerAccount) int { return strings.Compare(x.ID.String(), y.ID.String()) })
	for _, a := range accounts {
		if a.IsWalletAccount() {
			direct = append(direct, a)
		} else {
			shared = append(shared, a)
		}
	}
	return direct, shared
}

// Flush writes the unit of work's deferred postings to shared accounts now, for code that reads
// its own lines back before committing (nostro matching). Rows are then locked until commit.
func (l *Ledger) Flush(tx *store.Tx) error {
	if f, ok := tx.Value(ledgerFlushKey).(*ledgerFlush); ok {
		return f.write(tx)
	}
	return nil
}

// ledgerFlush holds a unit of work's postings to shared accounts until just before it commits.
type ledgerFlush struct {
	accounts  map[uuid.UUID]*domain.LedgerAccount
	deltas    map[uuid.UUID]int64
	lines     []*domain.JournalLine
	overdraft map[uuid.UUID]bool // allowed below zero by the posting that moved it
}

const ledgerFlushKey = "ledger.flush"

// flushFor returns the unit of work's deferred postings, registering their flush on first use;
// nil on a serial store.
func (l *Ledger) flushFor(tx *store.Tx) *ledgerFlush {
	if tx.Serial() {
		return nil
	}
	if f, ok := tx.Value(ledgerFlushKey).(*ledgerFlush); ok {
		return f
	}
	f := &ledgerFlush{accounts: map[uuid.UUID]*domain.LedgerAccount{}, deltas: map[uuid.UUID]int64{}, overdraft: map[uuid.UUID]bool{}}
	tx.SetValue(ledgerFlushKey, f)
	tx.BeforeCommit(f.write)
	return f
}

// defer_ keeps a line on a shared account for the flush; it reports whether it did.
func (f *ledgerFlush) defer_(line *domain.JournalLine, shared, overdraft []*domain.LedgerAccount) bool {
	for _, a := range shared {
		if a.ID != line.AccountID {
			continue
		}
		f.accounts[a.ID] = a
		f.deltas[a.ID] += a.SignedDelta(line.Direction, line.Amount)
		f.lines = append(f.lines, line)
		if slices.Contains(overdraft, a) || a.AllowNegative {
			f.overdraft[a.ID] = true
		}
		return true
	}
	return false
}

// write applies the deferred postings in ascending account id: each account's balance moves by
// an atomic increment, its true balance is read back under the row lock and checked, and its
// lines are written with their running balances.
//
// A split account that must not go negative is checked on its total, not part by part (ADR
// 0028). A unit of work taking money out of one takes the account's floor lock before any row,
// so those check one after the other; credits never take it.
func (f *ledgerFlush) write(tx *store.Tx) error {
	ids := make([]uuid.UUID, 0, len(f.deltas))
	for id := range f.deltas {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	var floors []uuid.UUID
	for _, id := range ids {
		if a := f.accounts[id]; a.Split() && !f.overdraft[id] && f.deltas[id] < 0 && !slices.Contains(floors, a.Group()) {
			floors = append(floors, a.Group())
		}
	}
	slices.SortFunc(floors, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
	for _, g := range floors {
		if err := tx.Lock("floor:" + g.String()); err != nil {
			return err
		}
	}
	running := map[uuid.UUID]int64{}
	for _, id := range ids {
		delta := f.deltas[id]
		if _, err := tx.Exec("UPDATE ledger_accounts SET balance = balance + ?, version = version + 1 WHERE id = ?", delta, id); err != nil {
			return err
		}
		now, err := store.Get[struct {
			Balance int64 `db:"balance"`
			Version int64 `db:"version"`
		}](tx, "SELECT balance, version FROM ledger_accounts WHERE id = ?", id)
		if err != nil {
			return err
		}
		a := f.accounts[id]
		if now.Balance < 0 && !f.overdraft[id] && !a.Split() {
			return domain.Rule("insufficient_funds", "Account %s has insufficient funds.", a.AccountNumber)
		}
		a.Balance, a.Version = now.Balance, now.Version
		running[id] = now.Balance - delta
	}
	for _, g := range floors {
		total, err := store.Get[struct {
			Number  string `db:"account_number"`
			Balance int64  `db:"balance"`
		}](tx, "SELECT p.account_number, (SELECT SUM(a.balance) FROM ledger_accounts a WHERE a.id = p.id OR a.parent_id = p.id) AS balance FROM ledger_accounts p WHERE p.id = ?", g)
		if err != nil {
			return err
		}
		if total.Balance < 0 {
			return domain.Rule("insufficient_funds", "Account %s has insufficient funds.", total.Number)
		}
	}
	for _, line := range f.lines {
		a := f.accounts[line.AccountID]
		running[a.ID] += a.SignedDelta(line.Direction, line.Amount)
		line.BalanceAfter = running[a.ID]
		if err := tx.Insert("journal_lines", line); err != nil {
			return err
		}
	}
	f.deltas, f.lines = map[uuid.UUID]int64{}, nil
	return nil
}
