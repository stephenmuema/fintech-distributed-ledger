package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/statements"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Nostro (NV) keeps our accounts at correspondents honest (T24: nostro reconciliation). Each
// nostro is mirrored by a ledger account; the correspondent's statements (MT940/MT950, camt.053
// or CSV) are matched to that account's postings line by line:
//
//  1. by reference — the statement quotes our transaction reference;
//  2. by amount and value date, when exactly one posting fits;
//  3. by the nostro's extensions (many-to-one, fuzzy), outside any write transaction;
//  4. by an operator.
//
// Statement entries our books still cannot explain after ParkAfterDays are parked to the
// nostro's suspense types — the books then reflect the correspondent, and the item is worked
// in suspense. Every statement carries a reconciliation proof.
type Nostro struct{ a *App }

// ---------------------------------------------------------------------------------------------
// Correspondents

func (n *Nostro) Correspondents(ctx context.Context) ([]domain.Correspondent, error) {
	return store.Select[domain.Correspondent](n.a.read(ctx), "SELECT * FROM correspondents ORDER BY code")
}

func (n *Nostro) loadCorrespondent(q store.Q, code string) (*domain.Correspondent, error) {
	c, err := store.Get[domain.Correspondent](q, "SELECT * FROM correspondents WHERE code = ?", code)
	if err == nil && c == nil {
		err = domain.NotFound("correspondent", code)
	}
	return c, err
}

// ProposeCorrespondent inputs a new or amended correspondent (INAU).
func (p *Parameters) ProposeCorrespondent(ctx context.Context, code string, spec domain.CorrespondentSpec) (*domain.ParameterChangeRequest, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if err := domain.ValidateCorrespondentCode(code); err != nil {
		return nil, err
	}
	if err := p.a.Nostro.checkCorrespondent(p.a.read(ctx), &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamCorrespondent, code, spec)
}

func (n *Nostro) checkCorrespondent(q store.Q, spec *domain.CorrespondentSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	if spec.Provider != "" && !n.a.providerNamed(spec.Provider) {
		return domain.Rule("unknown_provider", "No connector is configured as '%s'.", spec.Provider)
	}
	if spec.CustomerID != nil {
		c, err := store.Get[domain.Customer](q, "SELECT * FROM customers WHERE id = ?", *spec.CustomerID)
		if err != nil {
			return err
		}
		if c == nil {
			return domain.NotFound("customer", *spec.CustomerID)
		}
		if c.Type != domain.FinancialInstitution {
			return domain.Rule("not_a_financial_institution", "Customer %d is a %s; vostro holders are financial institutions.", c.CustomerNumber, c.Type)
		}
	}
	return nil
}

func (n *Nostro) applyCorrespondent(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.CorrespondentSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := n.checkCorrespondent(tx, &spec); err != nil {
		return err
	}
	c, err := store.Get[domain.Correspondent](tx, "SELECT * FROM correspondents WHERE code = ?", request.Key)
	if err != nil {
		return err
	}
	if c == nil {
		return tx.Insert("correspondents", domain.NewCorrespondent(request.Key, spec, tx.Now()))
	}
	c.Apply(spec)
	return tx.Update("correspondents", c)
}

// ---------------------------------------------------------------------------------------------
// Nostro accounts

func (n *Nostro) load(q store.Q, id uuid.UUID) (*domain.NostroAccount, error) {
	acct, err := store.Get[domain.NostroAccount](q, "SELECT * FROM nostro_accounts WHERE id = ?", id)
	if err == nil && acct == nil {
		err = domain.NotFound("nostro", id)
	}
	return acct, err
}

func (n *Nostro) loadByCode(q store.Q, code string) (*domain.NostroAccount, error) {
	acct, err := store.Get[domain.NostroAccount](q, "SELECT * FROM nostro_accounts WHERE code = ?", code)
	if err == nil && acct == nil {
		err = domain.NotFound("nostro", code)
	}
	return acct, err
}

// ProposeNostro inputs a new or amended nostro account (INAU).
func (p *Parameters) ProposeNostro(ctx context.Context, spec domain.NostroSpec) (*domain.ParameterChangeRequest, error) {
	if err := p.a.Nostro.checkNostro(p.a.read(ctx), &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamNostroAccount, domain.NostroCode(spec.Correspondent, spec.Currency), spec)
}

func (n *Nostro) checkNostro(q store.Q, spec *domain.NostroSpec) error {
	if err := spec.Normalize(); err != nil {
		return err
	}
	if _, err := n.loadCorrespondent(q, spec.Correspondent); err != nil {
		return err
	}
	for code, side := range map[string]domain.SuspenseSide{spec.CreditSuspenseType: domain.CreditSuspense, spec.DebitSuspenseType: domain.DebitSuspense} {
		t, err := n.a.Suspense.loadType(q, code)
		if err != nil {
			return err
		}
		if t.Side != side {
			return domain.Rule("invalid_nostro", "Suspense type %s is a %s suspense; it cannot take statement %ss.", code, t.Side, strings.ToLower(string(side)))
		}
		if !t.Supports(spec.Currency) {
			return domain.Rule("invalid_nostro", "Suspense type %s does not take %s.", code, spec.Currency)
		}
	}
	for _, h := range spec.Hooks {
		known, err := n.a.Extensions.Known(q, h)
		if err != nil {
			return err
		}
		if !known {
			return domain.Rule("unknown_extension", "Extension '%s' is neither compiled in nor a registered endpoint.", h)
		}
	}
	existing, err := store.Get[domain.NostroAccount](q, "SELECT * FROM nostro_accounts WHERE code = ?", domain.NostroCode(spec.Correspondent, spec.Currency))
	if err != nil {
		return err
	}
	if existing != nil {
		if spec.LedgerAccountNumber != "" && spec.LedgerAccountNumber != existing.LedgerAccountNumber {
			return domain.Rule("invalid_nostro", "A nostro's ledger account cannot change.")
		}
		return nil
	}
	if spec.LedgerAccountNumber == "" {
		return nil
	}
	acct, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE account_number = ?", spec.LedgerAccountNumber)
	if err != nil {
		return err
	}
	if acct == nil {
		return domain.NotFound("account", spec.LedgerAccountNumber)
	}
	if acct.Category != domain.CategoryNostro && acct.Category != domain.CategoryProviderFloat && acct.Category != domain.CategorySettlementBank {
		return domain.Rule("invalid_nostro", "%s is not a nostro, provider float or settlement bank account.", acct.AccountNumber)
	}
	if acct.Currency != spec.Currency {
		return domain.Rule("currency_mismatch", "%s is %s; the nostro is %s.", acct.AccountNumber, acct.Currency, spec.Currency)
	}
	taken, err := store.Exists(q, "SELECT 1 FROM nostro_accounts WHERE ledger_account_id = ?", acct.ID)
	if err != nil {
		return err
	}
	if taken {
		return domain.Conflict("ledger_account_taken", "%s already backs another nostro.", acct.AccountNumber)
	}
	return nil
}

func (n *Nostro) applyNostro(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.NostroSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := n.checkNostro(tx, &spec); err != nil {
		return err
	}
	existing, err := store.Get[domain.NostroAccount](tx, "SELECT * FROM nostro_accounts WHERE code = ?", request.Key)
	if err != nil {
		return err
	}
	if existing != nil {
		existing.Apply(spec)
		return tx.Update("nostro_accounts", existing)
	}
	var ledger *domain.LedgerAccount
	if spec.LedgerAccountNumber != "" {
		if ledger, err = n.a.Ledger.AccountByNumber(tx, spec.LedgerAccountNumber); err != nil {
			return err
		}
	} else if ledger, err = n.openLedgerAccount(tx, spec.Correspondent, spec.Currency); err != nil {
		return err
	}
	return tx.Insert("nostro_accounts", domain.NewNostroAccount(spec, ledger, tx.Now()))
}

// openLedgerAccount opens a new nostro account in category 14003, e.g. USD140030001.
func (n *Nostro) openLedgerAccount(tx *store.Tx, correspondent, currency string) (*domain.LedgerAccount, error) {
	spec := domain.NostroLedgerAccount(correspondent, currency)
	count, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM ledger_accounts WHERE category = ? AND currency = ?", spec.Category, currency)
	if err != nil {
		return nil, err
	}
	acct, err := domain.NewLedgerAccount(spec.Code, domain.InternalAccountNumber(spec.Category, currency, count+1), spec.Name, spec.Type, spec.Category, currency, spec.AllowNegative, tx.Now())
	if err != nil {
		return nil, err
	}
	return acct, tx.Insert("ledger_accounts", acct)
}

// ensureNostros adopts the platform's existing positions as nostros: one correspondent per
// provider and one for the settlement bank, with a nostro per currency that is the local
// currency or holds money.
func (n *Nostro) ensureNostros(tx *store.Tx) error {
	accounts, err := store.Select[domain.LedgerAccount](tx, accountColumns+" WHERE category IN (?, ?) ORDER BY code", domain.CategoryProviderFloat, domain.CategorySettlementBank)
	if err != nil {
		return err
	}
	for _, acct := range accounts {
		if acct.Currency != n.a.Options.LocalCurrency && acct.Balance == 0 {
			continue
		}
		taken, err := store.Exists(tx, "SELECT 1 FROM nostro_accounts WHERE ledger_account_id = ?", acct.ID)
		if err != nil {
			return err
		}
		if taken {
			continue
		}
		provider, code, kind, name := "", "SETTLEMENT", domain.CorrespondentBank, "Settlement bank"
		if acct.Category == domain.CategoryProviderFloat {
			provider = strings.TrimSuffix(strings.TrimPrefix(acct.Code, "asset:provider:"), ":"+acct.Currency)
			kind, err = n.kindOf(tx, provider)
			if err != nil {
				return err
			}
			code, name = strings.ToUpper(provider), provider
		}
		if domain.ValidateCorrespondentCode(code) != nil {
			continue
		}
		exists, err := store.Exists(tx, "SELECT 1 FROM correspondents WHERE code = ?", code)
		if err != nil {
			return err
		}
		if !exists {
			spec := domain.CorrespondentSpec{Name: name, Kind: kind, Country: n.a.Options.Country, Provider: provider}
			if err := spec.Normalize(); err != nil {
				return err
			}
			if err := tx.Insert("correspondents", domain.NewCorrespondent(code, spec, tx.Now())); err != nil {
				return err
			}
		}
		spec := domain.NostroSpec{Correspondent: code, Currency: acct.Currency}
		if err := spec.Normalize(); err != nil {
			return err
		}
		dup, err := store.Exists(tx, "SELECT 1 FROM nostro_accounts WHERE code = ?", domain.NostroCode(code, acct.Currency))
		if err != nil || dup {
			return err
		}
		if err := tx.Insert("nostro_accounts", domain.NewNostroAccount(spec, &acct, tx.Now())); err != nil {
			return err
		}
	}
	return nil
}

func (n *Nostro) kindOf(q store.Q, provider string) (domain.CorrespondentKind, error) {
	ch, err := store.Get[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE provider = ?", provider)
	if err != nil {
		return "", err
	}
	switch {
	case ch != nil && ch.Capabilities.BankCode != "":
		return domain.CorrespondentBank, nil
	case strings.Contains(provider, "mpesa") || strings.Contains(provider, "mobile"):
		return domain.CorrespondentMNO, nil
	case ch != nil && len(ch.Capabilities.Payouts) > 0 && ch.Capabilities.Payouts[0].Rail == domain.RailMobileMoney:
		return domain.CorrespondentMNO, nil
	}
	return domain.CorrespondentPSP, nil
}

// NostroView is a nostro with its position.
type NostroView struct {
	domain.NostroAccount
	CorrespondentName string                  `json:"correspondentName"`
	LedgerBalance     int64                   `json:"ledgerBalance"`
	LastStatement     *domain.NostroStatement `json:"lastStatement"`
	UnmatchedLines    int                     `json:"unmatchedStatementLines"`
	UnmatchedBook     int                     `json:"unmatchedBookEntries"`
	Live              bool                    `json:"live"`
	LiveBalance       *int64                  `json:"liveBalance"`
	LiveDifference    *int64                  `json:"liveDifference"`
	LiveError         string                  `json:"liveError,omitempty"`
}

// Accounts lists every nostro with its ledger position, latest statement and open items.
// withLive also asks connectors that support it for the live balance (outside any transaction).
func (n *Nostro) Accounts(ctx context.Context, withLive bool) ([]NostroView, error) {
	q := n.a.read(ctx)
	nostros, err := store.Select[domain.NostroAccount](q, "SELECT * FROM nostro_accounts ORDER BY code")
	if err != nil {
		return nil, err
	}
	views := []NostroView{}
	for _, nostro := range nostros {
		v, err := n.view(ctx, q, nostro, withLive)
		if err != nil {
			return nil, err
		}
		views = append(views, *v)
	}
	return views, nil
}

func (n *Nostro) Get(ctx context.Context, id uuid.UUID) (*NostroView, error) {
	q := n.a.read(ctx)
	nostro, err := n.load(q, id)
	if err != nil {
		return nil, err
	}
	return n.view(ctx, q, *nostro, true)
}

func (n *Nostro) view(ctx context.Context, q store.Q, nostro domain.NostroAccount, withLive bool) (*NostroView, error) {
	v := &NostroView{NostroAccount: nostro}
	c, err := n.loadCorrespondent(q, nostro.CorrespondentCode)
	if err != nil {
		return nil, err
	}
	v.CorrespondentName = c.Name
	acct, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE id = ?", nostro.LedgerAccountID)
	if err != nil {
		return nil, err
	}
	v.LedgerBalance = acct.Balance
	if v.LastStatement, err = store.Get[domain.NostroStatement](q, "SELECT * FROM nostro_statements WHERE nostro_id = ? ORDER BY to_date DESC, received_at DESC", nostro.ID); err != nil {
		return nil, err
	}
	if v.UnmatchedLines, err = store.Scalar[int](q, "SELECT COUNT(*) FROM nostro_statement_lines WHERE nostro_id = ? AND status = ?", nostro.ID, domain.LineUnmatched); err != nil {
		return nil, err
	}
	if nostro.ReconciledFrom != nil {
		if v.UnmatchedBook, err = store.Scalar[int](q, `SELECT COUNT(*) FROM journal_lines l WHERE l.account_id = ? AND l.booking_date >= ?
		    AND NOT EXISTS (SELECT 1 FROM nostro_ledger_links k WHERE k.journal_line_id = l.id)`, nostro.LedgerAccountID, *nostro.ReconciledFrom); err != nil {
			return nil, err
		}
	}
	if provider := n.a.Providers.Get(c.Provider); withLive && provider != nil {
		v.Live = provider.Live()
		if inquirer := provider; inquirer.Capabilities().Balance && v.Live {
			var balance int64
			err := n.a.ProviderCalls.Track(ctx, c.Provider, CallInfo{Operation: "balance_inquiry", SubjectType: "nostro", SubjectID: nostro.Code}, func(cc context.Context) (string, string, error) {
				var err error
				balance, err = inquirer.Balance(cc, nostro.Currency)
				return "succeeded", "", err
			})
			if err != nil {
				v.LiveError = err.Error()
			} else {
				diff := balance - acct.Balance
				v.LiveBalance, v.LiveDifference = &balance, &diff
			}
		}
	}
	return v, nil
}

// ---------------------------------------------------------------------------------------------
// Statements

// StatementUpload is a statement as received. Balances are only needed for CSV (MT940 and
// camt.053 carry their own); positive is a credit balance in our favour.
type StatementUpload struct {
	Format          string `json:"format"`
	Content         string `json:"content"`
	OpeningBalance  *int64 `json:"openingBalance"`
	ClosingBalance  *int64 `json:"closingBalance"`
	StatementNumber string `json:"statementNumber"`
}

// StatementDetail is a statement, its lines and the book entries still open on the nostro.
type StatementDetail struct {
	Statement   domain.NostroStatement       `json:"statement"`
	Lines       []domain.NostroStatementLine `json:"lines"`
	OpenBook    []BookLine                   `json:"openBookEntries"`
	Matches     []MatchView                  `json:"matches"`
	NostroCode  string                       `json:"nostroCode"`
	Reconciling *MatchRun                    `json:"reconciliation,omitempty"`
}

// BookLine is a posting on the nostro's ledger account.
type BookLine struct {
	LineID               uuid.UUID             `db:"line_id" json:"lineId"`
	EntryID              uuid.UUID             `db:"entry_id" json:"entryId"`
	TransactionReference string                `db:"transaction_reference" json:"transactionReference"`
	Reference            string                `db:"reference" json:"reference"`
	Description          string                `db:"description" json:"description"`
	Direction            domain.EntryDirection `db:"direction" json:"direction"`
	Amount               int64                 `db:"amount" json:"amount"`
	BookingDate          domain.Date           `db:"booking_date" json:"bookingDate"`
	ValueDate            domain.Date           `db:"value_date" json:"valueDate"`
}

// signedIn is the posting's effect on the nostro (an asset): debits add.
func (b BookLine) signedIn() int64 {
	if b.Direction == domain.Debit {
		return b.Amount
	}
	return -b.Amount
}

type MatchView struct {
	domain.NostroMatch
	StatementLineIDs []uuid.UUID `json:"statementLineIds"`
	BookLines        []BookLine  `json:"bookLines"`
}

// MatchRun counts what one reconciliation pass did.
type MatchRun struct {
	ByReference int `json:"byReference"`
	ByAmount    int `json:"byAmountAndDate"`
	ByExtension int `json:"byExtension"`
	Parked      int `json:"parked"`
}

// Ingest stores a statement, reconciles it and computes its proof. A statement is accepted
// once: re-sending the same file is a conflict.
func (n *Nostro) Ingest(ctx context.Context, nostroID uuid.UUID, up StatementUpload) (*StatementDetail, error) {
	q := n.a.read(ctx)
	nostro, err := n.load(q, nostroID)
	if err != nil {
		return nil, err
	}
	if !nostro.Active {
		return nil, domain.Rule("nostro_inactive", "Nostro %s is closed.", nostro.Code)
	}
	format, ok := domain.ParseEnum(strings.ToLower(strings.TrimSpace(up.Format)), statements.CSV, statements.MT940, statements.Camt053)
	if !ok {
		return nil, domain.Rule("invalid_statement", "Format must be csv, mt940 or camt053.")
	}
	st, err := statements.Parse(format, up.Content, nostro.Currency)
	if err != nil {
		return nil, err
	}
	if st.Account != "" && nostro.ExternalAccount != "" && !strings.EqualFold(strings.TrimSpace(st.Account), nostro.ExternalAccount) {
		return nil, domain.Rule("statement_account_mismatch", "The statement is for account %s; nostro %s is %s.", st.Account, nostro.Code, nostro.ExternalAccount)
	}
	if format == statements.CSV {
		if up.OpeningBalance != nil {
			st.Opening = &statements.Balance{Date: st.FromDate.AddDays(-1), Amount: *up.OpeningBalance}
		}
		if up.ClosingBalance != nil {
			st.Closing = &statements.Balance{Date: st.ToDate, Amount: *up.ClosingBalance}
		}
		if st.Number == "" {
			st.Number = up.StatementNumber
		}
	}
	sum := sha256.Sum256([]byte(up.Content))
	var statementID uuid.UUID
	err = n.a.db.Atomic(ctx, func(tx *store.Tx) error {
		nostro, err := n.load(tx, nostroID)
		if err != nil {
			return err
		}
		dup, err := store.Exists(tx, "SELECT 1 FROM nostro_statements WHERE nostro_id = ? AND content_hash = ?", nostro.ID, hex.EncodeToString(sum[:]))
		if err != nil {
			return err
		}
		if dup {
			return domain.Conflict("statement_duplicate", "This statement was already received for %s.", nostro.Code)
		}
		statement := &domain.NostroStatement{ID: domain.NewID(), NostroID: nostro.ID, NostroCode: nostro.Code, Format: string(format), StatementNumber: st.Number,
			Currency: nostro.Currency, FromDate: st.FromDate, ToDate: st.ToDate, LineCount: len(st.Lines), ContentHash: hex.EncodeToString(sum[:]),
			Status: domain.StatementOpen, ReceivedBy: tx.Actor(), ReceivedAt: tx.Now()}
		if st.Opening != nil {
			statement.OpeningBalance = &st.Opening.Amount
		}
		if st.Closing != nil {
			statement.ClosingBalance = &st.Closing.Amount
		}
		if err := tx.Insert("nostro_statements", statement); err != nil {
			return err
		}
		for i, l := range st.Lines {
			if err := tx.Insert("nostro_statement_lines", &domain.NostroStatementLine{ID: domain.NewID(), StatementID: statement.ID, NostroID: nostro.ID,
				LineNo: i + 1, ValueDate: l.ValueDate, EntryDate: l.EntryDate, Direction: l.Direction, Amount: l.Amount, BankReference: l.BankReference,
				CustomerReference: l.CustomerReference, Narrative: l.Narrative, Status: domain.LineUnmatched}); err != nil {
				return err
			}
		}
		if nostro.ReconciledFrom == nil {
			if err := n.cutOver(tx, nostro, statement); err != nil {
				return err
			}
		}
		statementID = statement.ID
		return tx.Audit("nostro.statement_received", "nostro_statement", statement.ID.String(), map[string]any{
			"nostro": nostro.Code, "format": format, "number": st.Number, "lines": len(st.Lines), "from": st.FromDate, "to": st.ToDate})
	})
	if err != nil {
		return nil, err
	}
	run, err := n.Reconcile(ctx, nostroID)
	if err != nil {
		return nil, err
	}
	if err := n.RefreshProofs(ctx, nostroID); err != nil {
		return nil, err
	}
	detail, err := n.Statement(ctx, statementID)
	if detail != nil {
		detail.Reconciling = &run
	}
	return detail, err
}

// cutOver starts reconciliation at the first statement: earlier book entries are history, and
// any gap between our balance and the correspondent's opening balance is the opening break.
func (n *Nostro) cutOver(tx *store.Tx, nostro *domain.NostroAccount, statement *domain.NostroStatement) error {
	from := statement.FromDate
	before, err := n.balanceAt(tx, nostro, from.AddDays(-1))
	if err != nil {
		return err
	}
	nostro.ReconciledFrom = &from
	if statement.OpeningBalance != nil {
		nostro.OpeningBreak = *statement.OpeningBalance - before
	}
	if err := tx.Update("nostro_accounts", nostro); err != nil {
		return err
	}
	return tx.Audit("nostro.cut_over", "nostro", nostro.Code, map[string]any{"reconciledFrom": from, "bookBalance": before, "openingBreak": nostro.OpeningBreak})
}

// balanceAt is the nostro's book balance at the end of date.
func (n *Nostro) balanceAt(q store.Q, nostro *domain.NostroAccount, date domain.Date) (int64, error) {
	acct, err := store.Get[domain.LedgerAccount](q, accountColumns+" WHERE id = ?", nostro.LedgerAccountID)
	if err != nil {
		return 0, err
	}
	later, err := store.Scalar[int64](q, "SELECT COALESCE(SUM(CASE direction WHEN 'Debit' THEN amount ELSE -amount END), 0) FROM journal_lines WHERE account_id = ? AND booking_date > ?",
		nostro.LedgerAccountID, date)
	return acct.Balance - later, err
}

func (n *Nostro) Statements(ctx context.Context, nostroID uuid.UUID) ([]domain.NostroStatement, error) {
	return store.Select[domain.NostroStatement](n.a.read(ctx), "SELECT * FROM nostro_statements WHERE nostro_id = ? ORDER BY to_date DESC, received_at DESC LIMIT 100", nostroID)
}

// Statement is one statement with its lines, matches and the nostro's open book entries.
func (n *Nostro) Statement(ctx context.Context, id uuid.UUID) (*StatementDetail, error) {
	q := n.a.read(ctx)
	statement, err := store.Get[domain.NostroStatement](q, "SELECT * FROM nostro_statements WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if statement == nil {
		return nil, domain.NotFound("nostro_statement", id)
	}
	nostro, err := n.load(q, statement.NostroID)
	if err != nil {
		return nil, err
	}
	d := &StatementDetail{Statement: *statement, NostroCode: nostro.Code, Matches: []MatchView{}}
	if d.Lines, err = store.Select[domain.NostroStatementLine](q, "SELECT * FROM nostro_statement_lines WHERE statement_id = ? ORDER BY line_no", id); err != nil {
		return nil, err
	}
	if d.OpenBook, err = n.openBook(q, nostro, nil); err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	for _, l := range d.Lines {
		if l.MatchID == nil || seen[*l.MatchID] {
			continue
		}
		seen[*l.MatchID] = true
		m, err := n.matchView(q, *l.MatchID)
		if err != nil {
			return nil, err
		}
		d.Matches = append(d.Matches, *m)
	}
	return d, nil
}

func (n *Nostro) matchView(q store.Q, id uuid.UUID) (*MatchView, error) {
	m, err := store.Get[domain.NostroMatch](q, "SELECT * FROM nostro_matches WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, domain.NotFound("nostro_match", id)
	}
	v := &MatchView{NostroMatch: *m}
	ids, err := store.Select[struct {
		ID uuid.UUID `db:"id"`
	}](q, "SELECT id FROM nostro_statement_lines WHERE match_id = ? ORDER BY line_no", id)
	if err != nil {
		return nil, err
	}
	for _, x := range ids {
		v.StatementLineIDs = append(v.StatementLineIDs, x.ID)
	}
	v.BookLines, err = store.Select[BookLine](q, bookLineColumns+" JOIN nostro_ledger_links k ON k.journal_line_id = l.id WHERE k.match_id = ? ORDER BY l.booking_date", id)
	return v, err
}

const bookLineColumns = `SELECT l.id AS line_id, e.id AS entry_id, e.transaction_reference, e.reference, e.description, l.direction, l.amount,
        l.booking_date, e.value_date FROM journal_lines l JOIN journal_entries e ON e.id = l.journal_entry_id`

// openBook returns the nostro's unexplained postings since cut-over (up to `to` when given).
func (n *Nostro) openBook(q store.Q, nostro *domain.NostroAccount, to *domain.Date) ([]BookLine, error) {
	if nostro.ReconciledFrom == nil {
		return []BookLine{}, nil
	}
	query := bookLineColumns + ` WHERE l.account_id = ? AND l.booking_date >= ?
	    AND NOT EXISTS (SELECT 1 FROM nostro_ledger_links k WHERE k.journal_line_id = l.id)`
	args := []any{nostro.LedgerAccountID, *nostro.ReconciledFrom}
	if to != nil {
		query += " AND l.booking_date <= ?"
		args = append(args, *to)
	}
	return store.Select[BookLine](q, query+" ORDER BY l.booking_date, e.posted_at LIMIT 2000", args...)
}

func (n *Nostro) openStatementLines(q store.Q, nostroID uuid.UUID) ([]domain.NostroStatementLine, error) {
	return store.Select[domain.NostroStatementLine](q, "SELECT * FROM nostro_statement_lines WHERE nostro_id = ? AND status = ? ORDER BY value_date, line_no LIMIT 2000",
		nostroID, domain.LineUnmatched)
}

// ---------------------------------------------------------------------------------------------
// Matching

type pairing struct {
	method     domain.MatchMethod
	by         string
	confidence *int
	statement  []uuid.UUID
	book       []uuid.UUID
}

// Reconcile matches open statement lines to open book entries: by reference, then by amount
// and value date, then through the nostro's extensions.
func (n *Nostro) Reconcile(ctx context.Context, nostroID uuid.UUID) (MatchRun, error) {
	var run MatchRun
	q := n.a.read(ctx)
	nostro, err := n.load(q, nostroID)
	if err != nil {
		return run, err
	}
	lines, err := n.openStatementLines(q, nostro.ID)
	if err != nil || len(lines) == 0 {
		return run, err
	}
	book, err := n.openBook(q, nostro, nil)
	if err != nil {
		return run, err
	}
	pairs := autoPairs(lines, book, nostro.ValueDateWindow)
	for _, p := range pairs {
		ok, err := n.applyPairing(ctx, nostro, p)
		if err != nil {
			return run, err
		}
		if ok && p.method == domain.MatchReference {
			run.ByReference++
		} else if ok {
			run.ByAmount++
		}
	}
	if len(nostro.Hooks) == 0 {
		return run, nil
	}
	if lines, err = n.openStatementLines(q, nostro.ID); err != nil || len(lines) == 0 {
		return run, err
	}
	if book, err = n.openBook(q, nostro, nil); err != nil {
		return run, err
	}
	matched, err := n.extensionPairs(ctx, nostro, lines, book)
	if err != nil {
		return run, err
	}
	for _, p := range matched {
		ok, err := n.applyPairing(ctx, nostro, p)
		if err != nil {
			return run, err
		}
		if ok {
			run.ByExtension++
		}
	}
	return run, nil
}

// autoPairs is the core's own one-to-one matching. A statement credit (money in) pairs with a
// book debit on the nostro, and the amounts must be equal.
func autoPairs(lines []domain.NostroStatementLine, book []BookLine, window int) []pairing {
	used := map[uuid.UUID]bool{}
	var pairs []pairing
	fits := func(l domain.NostroStatementLine, b BookLine) bool {
		return !used[b.LineID] && b.Amount == l.Amount && (l.Direction == "Credit") == (b.Direction == domain.Debit)
	}
	var rest []domain.NostroStatementLine
	for _, l := range lines {
		matched := false
		for _, b := range book {
			if fits(l, b) && quotes(l, b) {
				used[b.LineID] = true
				pairs = append(pairs, pairing{method: domain.MatchReference, by: STPActor, statement: []uuid.UUID{l.ID}, book: []uuid.UUID{b.LineID}})
				matched = true
				break
			}
		}
		if !matched {
			rest = append(rest, l)
		}
	}
	// Amount and value date: only when exactly one candidate fits on each side.
	candidates := map[uuid.UUID][]BookLine{}
	claims := map[uuid.UUID]int{}
	for _, l := range rest {
		for _, b := range book {
			if fits(l, b) && abs(l.ValueDate.DaysUntil(b.ValueDate)) <= window {
				candidates[l.ID] = append(candidates[l.ID], b)
				claims[b.LineID]++
			}
		}
	}
	for _, l := range rest {
		if c := candidates[l.ID]; len(c) == 1 && claims[c[0].LineID] == 1 {
			used[c[0].LineID] = true
			pairs = append(pairs, pairing{method: domain.MatchAmount, by: STPActor, statement: []uuid.UUID{l.ID}, book: []uuid.UUID{c[0].LineID}})
		}
	}
	return pairs
}

// quotes reports whether a statement line quotes one of our references.
func quotes(l domain.NostroStatementLine, b BookLine) bool {
	for _, ref := range []string{b.TransactionReference, b.Reference} {
		if ref == "" {
			continue
		}
		if strings.EqualFold(l.CustomerReference, ref) || strings.EqualFold(l.BankReference, ref) || strings.Contains(strings.ToUpper(l.Narrative), strings.ToUpper(ref)) {
			return true
		}
	}
	return false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (n *Nostro) extensionPairs(ctx context.Context, nostro *domain.NostroAccount, lines []domain.NostroStatementLine, book []BookLine) ([]pairing, error) {
	req := extapi.NostroMatchRequest{Nostro: extapi.Nostro{Code: nostro.Code, Correspondent: nostro.CorrespondentCode, Currency: nostro.Currency, ExternalAccount: nostro.ExternalAccount}}
	offeredLines, offeredBook := map[string]domain.NostroStatementLine{}, map[string]BookLine{}
	for _, l := range lines {
		offeredLines[l.ID.String()] = l
		req.StatementLines = append(req.StatementLines, extapi.StatementLine{ID: l.ID.String(), ValueDate: l.ValueDate.String(), Direction: l.Direction, Amount: l.Amount,
			BankReference: l.BankReference, CustomerReference: l.CustomerReference, Narrative: l.Narrative})
	}
	for _, b := range book {
		offeredBook[b.LineID.String()] = b
		req.LedgerLines = append(req.LedgerLines, extapi.LedgerLine{ID: b.LineID.String(), TransactionReference: b.TransactionReference, Reference: b.Reference,
			Direction: string(b.Direction), Amount: b.Amount, BookingDate: b.BookingDate.String(), Description: b.Description})
	}
	answers, err := consult[extapi.NostroMatchResponse](ctx, n.a.Extensions, nostro.Hooks, extapi.NostroMatch, nostro.Code, req)
	if err != nil {
		return nil, err
	}
	var pairs []pairing
	for _, a := range answers {
		for _, m := range a.Response.Matches {
			p := pairing{method: domain.MatchExtension, by: "hook:" + a.Extension}
			valid := len(m.StatementLineIDs)+len(m.LedgerLineIDs) > 0
			for _, id := range m.StatementLineIDs {
				l, ok := offeredLines[id]
				valid = valid && ok
				p.statement = append(p.statement, l.ID)
			}
			for _, id := range m.LedgerLineIDs {
				b, ok := offeredBook[id]
				valid = valid && ok
				p.book = append(p.book, b.LineID)
			}
			if !valid {
				n.a.Logger.Warn("extension proposed a nostro match on lines it was not offered", "extension", a.Extension, "nostro", nostro.Code)
				continue
			}
			confidence := int(math.Round(math.Max(0, math.Min(1, m.Confidence)) * 100))
			p.confidence = &confidence
			pairs = append(pairs, p)
		}
	}
	return pairs, nil
}

// applyPairing records one match in its own unit of work after re-checking every line is still
// open and the group balances. It reports false when the lines moved on in the meantime.
func (n *Nostro) applyPairing(ctx context.Context, nostro *domain.NostroAccount, p pairing) (bool, error) {
	err := n.a.db.Atomic(ctx, func(tx *store.Tx) error {
		_, err := n.match(tx, nostro, p)
		return err
	})
	if code := domain.CodeOf(err); code == "nostro_line_not_open" || code == "nostro_match_unbalanced" {
		n.a.Logger.Info("nostro pairing skipped", "nostro", nostro.Code, "method", p.method, "reason", err)
		return false, nil
	}
	return err == nil, err
}

func (n *Nostro) match(tx *store.Tx, nostro *domain.NostroAccount, p pairing) (*domain.NostroMatch, error) {
	if err := n.a.Ledger.Flush(tx); err != nil { // the book lines it pairs may have been posted in this unit of work
		return nil, err
	}
	if len(p.statement)+len(p.book) == 0 {
		return nil, domain.Rule("nostro_match_empty", "A match needs at least one line.")
	}
	var statementIn, bookIn int64
	lines := make([]*domain.NostroStatementLine, 0, len(p.statement))
	for _, id := range p.statement {
		l, err := store.Get[domain.NostroStatementLine](tx, "SELECT * FROM nostro_statement_lines WHERE id = ?", id)
		if err != nil {
			return nil, err
		}
		if l == nil || l.NostroID != nostro.ID || l.Status != domain.LineUnmatched {
			return nil, domain.Conflict("nostro_line_not_open", "Statement line %s is not open on %s.", id, nostro.Code)
		}
		statementIn += l.SignedIn()
		lines = append(lines, l)
	}
	for _, id := range p.book {
		b, err := store.Get[BookLine](tx, bookLineColumns+" WHERE l.id = ? AND l.account_id = ?", id, nostro.LedgerAccountID)
		if err != nil {
			return nil, err
		}
		linked, err := store.Exists(tx, "SELECT 1 FROM nostro_ledger_links WHERE journal_line_id = ?", id)
		if err != nil {
			return nil, err
		}
		if b == nil || linked {
			return nil, domain.Conflict("nostro_line_not_open", "Book entry %s is not open on %s.", id, nostro.Code)
		}
		bookIn += b.signedIn()
	}
	if statementIn != bookIn {
		return nil, domain.Rule("nostro_match_unbalanced", "Statement lines net %d but book entries net %d; a match must agree exactly.", statementIn, bookIn)
	}
	m := &domain.NostroMatch{ID: domain.NewID(), NostroID: nostro.ID, Method: p.method, MatchedBy: p.by, Confidence: p.confidence, CreatedAt: tx.Now()}
	if m.MatchedBy == "" {
		m.MatchedBy = tx.Actor()
	}
	if err := tx.Insert("nostro_matches", m); err != nil {
		return nil, err
	}
	for _, id := range p.book {
		if err := tx.Insert("nostro_ledger_links", &domain.NostroLedgerLink{JournalLineID: id, MatchID: m.ID, NostroID: nostro.ID}); err != nil {
			if store.IsUniqueViolation(err) {
				return nil, domain.Conflict("nostro_line_not_open", "Book entry %s was matched concurrently.", id)
			}
			return nil, err
		}
	}
	status := domain.LineMatched
	if p.method == domain.MatchParked {
		status = domain.LineParked
	}
	for _, l := range lines {
		l.Status, l.MatchID = status, &m.ID
		if err := tx.Update("nostro_statement_lines", l); err != nil {
			return nil, err
		}
	}
	return m, tx.Audit("nostro.matched", "nostro_match", m.ID.String(), map[string]any{
		"nostro": nostro.Code, "method": p.method, "by": m.MatchedBy, "statementLines": p.statement, "bookEntries": p.book, "confidence": p.confidence})
}

// MatchManually is an operator's match. Any group is allowed as long as money in and money out
// agree exactly — including book entries that cancel each other.
func (n *Nostro) MatchManually(ctx context.Context, nostroID uuid.UUID, statementLines, bookLines []uuid.UUID) (*MatchView, error) {
	var m *domain.NostroMatch
	err := n.a.db.Atomic(ctx, func(tx *store.Tx) error {
		nostro, err := n.load(tx, nostroID)
		if err != nil {
			return err
		}
		m, err = n.match(tx, nostro, pairing{method: domain.MatchManual, statement: statementLines, book: bookLines})
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := n.RefreshProofs(ctx, nostroID); err != nil {
		return nil, err
	}
	return n.matchView(n.a.read(ctx), m.ID)
}

// Unmatch reopens a match's lines. A parked line cannot be unmatched: its posting exists, so it
// is worked as a suspense item instead.
func (n *Nostro) Unmatch(ctx context.Context, nostroID, matchID uuid.UUID) error {
	err := n.a.db.Atomic(ctx, func(tx *store.Tx) error {
		m, err := store.Get[domain.NostroMatch](tx, "SELECT * FROM nostro_matches WHERE id = ? AND nostro_id = ?", matchID, nostroID)
		if err != nil {
			return err
		}
		if m == nil {
			return domain.NotFound("nostro_match", matchID)
		}
		if m.Method == domain.MatchParked {
			return domain.Rule("parked_line", "A parked line is cleared through its suspense item, not unmatched.")
		}
		lines, err := store.Select[domain.NostroStatementLine](tx, "SELECT * FROM nostro_statement_lines WHERE match_id = ?", matchID)
		if err != nil {
			return err
		}
		for i := range lines {
			lines[i].Status, lines[i].MatchID = domain.LineUnmatched, nil
			if err := tx.Update("nostro_statement_lines", &lines[i]); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM nostro_ledger_links WHERE match_id = ?", matchID); err != nil {
			return err
		}
		return tx.Audit("nostro.unmatched", "nostro_match", matchID.String(), map[string]any{"method": m.Method, "matchedBy": m.MatchedBy})
	})
	if err != nil {
		return err
	}
	return n.RefreshProofs(ctx, nostroID)
}

// ---------------------------------------------------------------------------------------------
// Parking breaks to suspense

// Park books a statement line our books cannot explain into the nostro's suspense type:
// a credit (money in) → Dr nostro / Cr credit suspense; a debit → Dr debit suspense / Cr nostro.
// The nostro leg is matched to the line at once, so the line is explained and the item is
// worked in suspense.
func (n *Nostro) Park(ctx context.Context, nostroID, lineID uuid.UUID) (*domain.NostroStatementLine, error) {
	var line *domain.NostroStatementLine
	err := n.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		line, err = n.park(tx, nostroID, lineID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return line, n.RefreshProofs(ctx, nostroID)
}

func (n *Nostro) park(tx *store.Tx, nostroID, lineID uuid.UUID) (*domain.NostroStatementLine, error) {
	nostro, err := n.load(tx, nostroID)
	if err != nil {
		return nil, err
	}
	line, err := store.Get[domain.NostroStatementLine](tx, "SELECT * FROM nostro_statement_lines WHERE id = ? AND nostro_id = ?", lineID, nostroID)
	if err != nil {
		return nil, err
	}
	if line == nil {
		return nil, domain.NotFound("nostro_statement_line", lineID)
	}
	if line.Status != domain.LineUnmatched {
		return nil, domain.Conflict("nostro_line_not_open", "Statement line %d is %s.", line.LineNo, line.Status)
	}
	typeCode := nostro.CreditSuspenseType
	if line.Direction == "Debit" {
		typeCode = nostro.DebitSuspenseType
	}
	t, err := n.a.Suspense.loadType(tx, typeCode)
	if err != nil {
		return nil, err
	}
	ledger, err := n.a.Ledger.Account(tx, nostro.LedgerAccountID)
	if err != nil {
		return nil, err
	}
	reference := firstNonBlank(line.CustomerReference, line.BankReference, fmt.Sprintf("%s line %d", nostro.Code, line.LineNo))
	attrs := domain.Attributes{}
	for _, f := range t.LocalFields {
		switch f.Name {
		case "nostro":
			attrs[f.Name] = nostro.Code
		case "bank_reference":
			if line.BankReference != "" {
				attrs[f.Name] = line.BankReference
			}
		case "value_date":
			attrs[f.Name] = line.ValueDate.String()
		}
	}
	narrative := strings.TrimSpace(fmt.Sprintf("%s statement %s: %s", nostro.Code, strings.ToLower(line.Direction), firstNonBlank(line.Narrative, reference)))
	item, entry, err := n.a.Suspense.Park(tx, ParkRequest{Type: t.Code, Currency: nostro.Currency, Amount: line.Amount, Contra: ledger,
		Reference: line.ParkReference(), Source: ItemSource{Type: sourceNostroLine, ID: line.ID.String(), Reference: reference, Narrative: narrative, Attributes: attrs}})
	if err != nil {
		return nil, err
	}
	var nostroLeg uuid.UUID
	for _, l := range entry.Lines {
		if l.AccountID == nostro.LedgerAccountID {
			nostroLeg = l.ID
		}
	}
	if _, err := n.match(tx, nostro, pairing{method: domain.MatchParked, statement: []uuid.UUID{line.ID}, book: []uuid.UUID{nostroLeg}}); err != nil {
		return nil, err
	}
	line, err = store.Get[domain.NostroStatementLine](tx, "SELECT * FROM nostro_statement_lines WHERE id = ?", lineID)
	if err != nil {
		return nil, err
	}
	line.SuspenseItemID = &item.ID
	return line, tx.Update("nostro_statement_lines", line)
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ParkDue parks every statement line still unexplained ParkAfterDays after its value date.
func (n *Nostro) ParkDue(ctx context.Context, nostroID uuid.UUID, today domain.Date) (int, error) {
	q := n.a.read(ctx)
	nostro, err := n.load(q, nostroID)
	if err != nil {
		return 0, err
	}
	due, err := store.Select[domain.NostroStatementLine](q, "SELECT * FROM nostro_statement_lines WHERE nostro_id = ? AND status = ? AND value_date <= ? ORDER BY value_date, line_no",
		nostro.ID, domain.LineUnmatched, today.AddDays(-nostro.ParkAfterDays))
	if err != nil {
		return 0, err
	}
	parked := 0
	for _, l := range due {
		err := n.a.db.Atomic(ctx, func(tx *store.Tx) error {
			_, err := n.park(tx, nostro.ID, l.ID)
			return err
		})
		if domain.CodeOf(err) == "nostro_line_not_open" {
			continue
		}
		if err != nil {
			return parked, err
		}
		parked++
	}
	return parked, nil
}

// ---------------------------------------------------------------------------------------------
// Proof

// RefreshProofs recomputes the proof of every open statement of a nostro; a statement whose
// proof closes with all its lines explained is Reconciled.
func (n *Nostro) RefreshProofs(ctx context.Context, nostroID uuid.UUID) error {
	return n.a.db.Atomic(ctx, func(tx *store.Tx) error {
		nostro, err := n.load(tx, nostroID)
		if err != nil {
			return err
		}
		open, err := store.Select[domain.NostroStatement](tx, "SELECT * FROM nostro_statements WHERE nostro_id = ? AND status = ?", nostroID, domain.StatementOpen)
		if err != nil {
			return err
		}
		for i := range open {
			s := &open[i]
			proof, err := n.proof(tx, nostro, s)
			if err != nil {
				return err
			}
			s.Proof = proof
			unexplained, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM nostro_statement_lines WHERE statement_id = ? AND status = ?", s.ID, domain.LineUnmatched)
			if err != nil {
				return err
			}
			if proof != nil && proof.Closes && unexplained == 0 {
				s.Status = domain.StatementReconciled
				if err := tx.Audit("nostro.statement_reconciled", "nostro_statement", s.ID.String(), proof); err != nil {
					return err
				}
			}
			if err := tx.Update("nostro_statements", s); err != nil {
				return err
			}
		}
		return nil
	})
}

func (n *Nostro) proof(q store.Q, nostro *domain.NostroAccount, s *domain.NostroStatement) (*domain.Proof, error) {
	if s.ClosingBalance == nil {
		return nil, nil
	}
	balance, err := n.balanceAt(q, nostro, s.ToDate)
	if err != nil {
		return nil, err
	}
	p := &domain.Proof{AsOf: s.ToDate, LedgerBalance: balance, OpeningBreak: nostro.OpeningBreak, StatementClosing: *s.ClosingBalance}
	lines, err := store.Select[domain.NostroStatementLine](q, "SELECT * FROM nostro_statement_lines WHERE nostro_id = ? AND status = ? AND value_date <= ?",
		nostro.ID, domain.LineUnmatched, s.ToDate)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		if l.Direction == "Credit" {
			p.UnmatchedStmtCredits += l.Amount
		} else {
			p.UnmatchedStmtDebits += l.Amount
		}
	}
	book, err := n.openBook(q, nostro, &s.ToDate)
	if err != nil {
		return nil, err
	}
	for _, b := range book {
		if b.Direction == domain.Debit {
			p.UnmatchedBookDebits += b.Amount
		} else {
			p.UnmatchedBookCredits += b.Amount
		}
	}
	p.Compute()
	return p, nil
}

// ---------------------------------------------------------------------------------------------
// Inbox and COB

var statementFile = regexp.MustCompile(`^([A-Z][A-Z0-9-]*-[A-Z]{3})_[^/]*\.(mt940|sta|txt|xml|csv)$`)

// ImportInbox ingests the statements waiting in the inbox: <NOSTRO-CODE>_<anything>.<ext>.
// Files land in processed/ or rejected/ afterwards.
func (n *Nostro) ImportInbox(ctx context.Context, dir string) (imported, rejected int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, sub := range []string{"processed", "rejected"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return 0, 0, err
		}
	}
	stp := store.WithActor(ctx, STPActor)
	for _, entry := range entries {
		m := statementFile.FindStringSubmatch(entry.Name())
		if entry.IsDir() || m == nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path) // #nosec G304 -- the name matched statementFile inside the configured inbox
		if err != nil {
			return imported, rejected, err
		}
		format := map[string]string{"mt940": "mt940", "sta": "mt940", "txt": "mt940", "xml": "camt053", "csv": "csv"}[m[2]]
		reason := ""
		nostro, err := n.loadByCode(n.a.read(ctx), m[1])
		if err == nil {
			_, err = n.Ingest(stp, nostro.ID, StatementUpload{Format: format, Content: string(raw)})
		}
		if _, isRule := domain.AsError(err); isRule {
			reason, err = err.Error(), nil
		}
		if err != nil {
			return imported, rejected, err
		}
		target := "processed"
		if reason != "" {
			target = "rejected"
			rejected++
			n.a.Logger.Warn("nostro statement rejected", "file", entry.Name(), "reason", reason)
			if err := n.a.db.Atomic(stp, func(tx *store.Tx) error {
				return tx.Audit("nostro.statement_rejected", "statement_file", entry.Name(), map[string]any{"reason": reason})
			}); err != nil {
				return imported, rejected, err
			}
		} else {
			imported++
		}
		if err := os.Rename(path, filepath.Join(dir, target, entry.Name())); err != nil {
			return imported, rejected, err
		}
	}
	return imported, rejected, nil
}

func (c *Cob) nostroInboxJob() CobJob {
	return NewCobJob("NV.STATEMENT.INBOX", "NV", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		if c.a.Options.StatementInbox == "" {
			return CobJobResult{0, "no statement inbox configured"}, nil
		}
		imported, rejected, err := c.a.Nostro.ImportInbox(ctx, c.a.Options.StatementInbox)
		return CobJobResult{imported, fmt.Sprintf("%d statement(s) imported, %d rejected", imported, rejected)}, err
	})
}

// nostroMatchJob re-runs matching (book entries may have arrived since the statement), parks
// breaks that are due, and refreshes every proof.
func (c *Cob) nostroMatchJob() CobJob {
	return NewCobJob("NV.AUTO.MATCH", "NV", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		nostros, err := store.Select[domain.NostroAccount](c.a.read(ctx), "SELECT * FROM nostro_accounts WHERE active = 1 ORDER BY code")
		if err != nil {
			return CobJobResult{}, err
		}
		var total MatchRun
		for _, nostro := range nostros {
			run, err := c.a.Nostro.Reconcile(ctx, nostro.ID)
			if err != nil {
				return CobJobResult{}, err
			}
			parked, err := c.a.Nostro.ParkDue(ctx, nostro.ID, cc.BusinessDate)
			if err != nil {
				return CobJobResult{}, err
			}
			if err := c.a.Nostro.RefreshProofs(ctx, nostro.ID); err != nil {
				return CobJobResult{}, err
			}
			total.ByReference += run.ByReference
			total.ByAmount += run.ByAmount
			total.ByExtension += run.ByExtension
			total.Parked += parked
		}
		matched := total.ByReference + total.ByAmount + total.ByExtension
		return CobJobResult{matched + total.Parked, fmt.Sprintf("%d matched (%d by reference, %d by amount, %d by extension), %d parked to suspense",
			matched, total.ByReference, total.ByAmount, total.ByExtension, total.Parked)}, nil
	})
}

// nostroReconCheckJob reports statements whose proof does not close, and halts COB for nostros
// whose proof policy demands it.
func (c *Cob) nostroReconCheckJob() CobJob {
	return NewCobJob("NV.RECON.CHECK", "NV", domain.StageReporting, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		q := c.a.read(ctx)
		rows, err := store.Select[struct {
			Code   string           `db:"nostro_code"`
			Policy domain.CobPolicy `db:"proof_policy"`
			Proof  *domain.Proof    `db:"proof"`
		}](q, `SELECT s.nostro_code, n.proof_policy, s.proof FROM nostro_statements s JOIN nostro_accounts n ON n.id = s.nostro_id
		     WHERE s.status = ? ORDER BY s.nostro_code`, domain.StatementOpen)
		if err != nil {
			return CobJobResult{}, err
		}
		var breaks, blocking []string
		for _, r := range rows {
			if r.Proof == nil || r.Proof.Closes {
				continue
			}
			msg := fmt.Sprintf("%s (difference %d)", r.Code, r.Proof.Difference)
			breaks = append(breaks, msg)
			if r.Policy == domain.PolicyHaltCob {
				blocking = append(blocking, msg)
			}
		}
		sort.Strings(blocking)
		if len(blocking) > 0 {
			return CobJobResult{}, fmt.Errorf("nostro proofs do not close: %s", strings.Join(blocking, ", "))
		}
		if len(breaks) == 0 {
			return CobJobResult{len(rows), fmt.Sprintf("%d open statement(s); every proof closes", len(rows))}, nil
		}
		return CobJobResult{len(rows), "proofs not closing: " + strings.Join(breaks, ", ")}, nil
	})
}
