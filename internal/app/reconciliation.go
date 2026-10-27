package app

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Reconciliation is daily provider reconciliation and settlement (T24: nostro reconciliation):
//  1. ingest the provider's settlement report (CSV);
//  2. match it line by line against succeeded charges, refunds and paid payouts;
//  3. operators resolve breaks with a note (audited);
//  4. settle: move net proceeds from the provider float to the bank and book provider fees.
type Reconciliation struct{ a *App }

// ParseSettlementCSV parses provider_reference,type,amount,fee,currency lines (header optional).
func ParseSettlementCSV(csv string) ([]domain.SettlementReportLine, error) {
	var lines []domain.SettlementReportLine
	number := 0
	for _, raw := range strings.Split(csv, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		number++
		cols := strings.Split(raw, ",")
		for i := range cols {
			cols[i] = strings.TrimSpace(cols[i])
		}
		if number == 1 && strings.EqualFold(cols[0], "provider_reference") {
			continue
		}
		malformed := domain.Rule("invalid_report", "Line %d is malformed: '%s'.", number, raw)
		if len(cols) != 5 {
			return nil, malformed
		}
		kind, ok := domain.ParseEnum(cols[1], domain.ReconCharge, domain.ReconRefund, domain.ReconPayout)
		amount, amountErr := strconv.ParseInt(cols[2], 10, 64)
		fee, feeErr := strconv.ParseInt(cols[3], 10, 64)
		currency := strings.ToUpper(cols[4])
		if !ok || amountErr != nil || feeErr != nil || amount < 0 || fee < 0 || !domain.IsSupportedCurrency(currency) {
			return nil, malformed
		}
		lines = append(lines, domain.SettlementReportLine{ProviderReference: cols[0], Kind: kind, Amount: amount, Fee: fee, Currency: currency})
	}
	return lines, nil
}

func (s *Reconciliation) Run(ctx context.Context, provider string, reportDate domain.Date, report []domain.SettlementReportLine) (*domain.ReconciliationRun, error) {
	var run *domain.ReconciliationRun
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		exists, err := store.Exists(tx, "SELECT 1 FROM reconciliation_runs WHERE provider = ? AND report_date = ?", provider, reportDate)
		if err != nil {
			return err
		}
		if exists {
			return domain.Conflict("run_exists", "%s has already been reconciled for %s.", provider, reportDate)
		}
		from := reportDate.Time()
		to := from.Add(24 * time.Hour)

		type record struct {
			Reference string `db:"reference"`
			Amount    int64  `db:"amount"`
			Currency  string `db:"currency"`
		}
		var ledger []domain.LedgerRecord
		sources := []struct {
			kind  domain.ReconLineKind
			query string
			args  []any
		}{
			{domain.ReconCharge, "SELECT provider_reference AS reference, amount, currency FROM payments WHERE provider = ? AND status = ? AND succeeded_at >= ? AND succeeded_at < ? ORDER BY succeeded_at",
				[]any{provider, domain.PaymentSucceeded, from, to}},
			{domain.ReconRefund, "SELECT provider_reference AS reference, amount, currency FROM refunds WHERE provider = ? AND status = ? AND completed_at >= ? AND completed_at < ? ORDER BY completed_at",
				[]any{provider, domain.RefundSucceeded, from, to}},
			{domain.ReconPayout, "SELECT provider_reference AS reference, amount, currency FROM payouts WHERE provider = ? AND status = ? AND completed_at >= ? AND completed_at < ? ORDER BY completed_at",
				[]any{provider, domain.PayoutPaid, from, to}},
		}
		for _, src := range sources {
			rows, err := store.Select[record](tx, src.query, src.args...)
			if err != nil {
				return err
			}
			for _, r := range rows {
				ledger = append(ledger, domain.LedgerRecord{ProviderReference: r.Reference, Kind: src.kind, Amount: r.Amount, Currency: r.Currency})
			}
		}

		if run, err = domain.ExecuteReconciliation(provider, reportDate, report, ledger, tx.Actor(), tx.Now()); err != nil {
			return err
		}
		if err := tx.Insert("reconciliation_runs", run); err != nil {
			return err
		}
		for i := range run.Items {
			if err := tx.Insert("reconciliation_items", &run.Items[i]); err != nil {
				return err
			}
		}
		breaks := len(run.Items) - run.MatchedCount()
		s.a.Metrics.ReconExceptions.Add(float64(breaks), "provider", provider)
		return tx.Audit("reconciliation.run", "reconciliation_run", run.ID.String(),
			map[string]any{"provider": provider, "reportDate": reportDate, "matched": run.MatchedCount(), "breaks": breaks})
	})
	return run, err
}

func (s *Reconciliation) load(q store.Q, id uuid.UUID) (*domain.ReconciliationRun, error) {
	run, err := store.Get[domain.ReconciliationRun](q, "SELECT * FROM reconciliation_runs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, domain.NotFound("reconciliation_run", id)
	}
	run.Items, err = store.Select[domain.ReconciliationItem](q, "SELECT * FROM reconciliation_items WHERE run_id = ? ORDER BY id", id)
	return run, err
}

func (s *Reconciliation) Get(ctx context.Context, id uuid.UUID) (*domain.ReconciliationRun, error) {
	return s.load(s.a.read(ctx), id)
}

func (s *Reconciliation) List(ctx context.Context) ([]domain.ReconciliationRun, error) {
	q := s.a.read(ctx)
	runs, err := store.Select[domain.ReconciliationRun](q, "SELECT * FROM reconciliation_runs ORDER BY report_date DESC LIMIT 60")
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if runs[i].Items, err = store.Select[domain.ReconciliationItem](q, "SELECT * FROM reconciliation_items WHERE run_id = ? ORDER BY id", runs[i].ID); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func (s *Reconciliation) Resolve(ctx context.Context, runID, itemID uuid.UUID, note string) (*domain.ReconciliationItem, error) {
	var item *domain.ReconciliationItem
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if item, err = store.Get[domain.ReconciliationItem](tx, "SELECT * FROM reconciliation_items WHERE id = ? AND run_id = ?", itemID, runID); err != nil {
			return err
		}
		if item == nil {
			return domain.NotFound("reconciliation_item", itemID)
		}
		if err := item.Resolve(note, tx.Actor(), tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("reconciliation_items", item); err != nil {
			return err
		}
		return tx.Audit("reconciliation.exception_resolved", "reconciliation_item", itemID.String(),
			map[string]any{"runId": runID, "providerReference": item.ProviderReference, "outcome": item.Outcome, "note": note})
	})
	return item, err
}

// Settle posts matched lines, per currency:
// Dr settlement bank (net) / Dr processing costs (provider fees) / Cr provider float (charges − refunds).
// Settle posts a run's settlement under the caller's name: COB does it straight through for
// clean runs (SettleClean); operators do it under maker-checker (Parameters.ProposeSettlement).
func (s *Reconciliation) Settle(ctx context.Context, runID uuid.UUID) (*domain.ReconciliationRun, error) {
	var run *domain.ReconciliationRun
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		run, err = s.settle(tx, runID)
		return err
	})
	return run, err
}

func (s *Reconciliation) settle(tx *store.Tx, runID uuid.UUID) (*domain.ReconciliationRun, error) {
	run, err := s.load(tx, runID)
	if err != nil {
		return nil, err
	}
	type totals struct{ gross, fees int64 }
	byCurrency := map[string]*totals{}
	for _, item := range run.Items {
		if item.Outcome != domain.ReconMatched || item.Kind == domain.ReconPayout {
			continue
		}
		t := byCurrency[item.Currency]
		if t == nil {
			t = &totals{}
			byCurrency[item.Currency] = t
		}
		amount := derefInt(item.ReportAmount)
		if item.Kind == domain.ReconRefund {
			amount = -amount
		}
		t.gross += amount
		t.fees += derefInt(item.ReportFee)
	}

	currencies := make([]string, 0, len(byCurrency))
	for c := range byCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	builder := domain.NewEntry("settlement:"+domain.N(run.ID), domain.CatSettlement, run.Provider+" settlement for "+run.ReportDate.String())
	for _, currency := range currencies {
		t := byCurrency[currency]
		bank, err := s.a.Ledger.SystemAccount(tx, domain.SettlementBank(currency))
		if err != nil {
			return nil, err
		}
		costs, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFees(currency))
		if err != nil {
			return nil, err
		}
		providerFloat, err := s.a.Ledger.SystemAccount(tx, domain.ProviderFloat(run.Provider, currency))
		if err != nil {
			return nil, err
		}
		builder.DebitSigned(bank, t.gross-t.fees).Debit(costs, t.fees).DebitSigned(providerFloat, -t.gross)
	}
	if !builder.HasLegs() {
		return nil, domain.Rule("nothing_to_settle", "No matched charges or refunds to settle.")
	}
	if err := run.MarkSettled(domain.NewID(), tx.Now()); err != nil { // validates before posting
		return nil, err
	}
	entry, err := s.a.Ledger.Post(tx, builder)
	if err != nil {
		return nil, err
	}
	run.SettlementEntryID = &entry.ID
	if _, err := tx.Exec("UPDATE reconciliation_runs SET settlement_entry_id = ?, settled_at = ? WHERE id = ?", entry.ID, run.SettledAt, run.ID); err != nil {
		return nil, err
	}
	// The provider has now paid these collections over: their merchants need not wait longer.
	var settled []string
	for _, item := range run.Items {
		if item.Outcome == domain.ReconMatched && item.Kind == domain.ReconCharge {
			settled = append(settled, item.ProviderReference)
		}
	}
	if err := s.a.Payments.releaseSettled(tx, run.Provider, settled); err != nil {
		return nil, err
	}
	return run, tx.Audit("reconciliation.settled", "reconciliation_run", run.ID.String(), map[string]any{"transactionReference": entry.TransactionReference})
}

// SettleClean settles every unsettled run with no open breaks: matched in full, or with every
// break resolved by an operator. Runs with nothing to settle (payouts only) are left alone.
func (s *Reconciliation) SettleClean(ctx context.Context) (int, error) {
	q := s.a.read(ctx)
	runs, err := store.Select[domain.ReconciliationRun](q, "SELECT * FROM reconciliation_runs WHERE settlement_entry_id IS NULL ORDER BY report_date, id LIMIT 100")
	if err != nil {
		return 0, err
	}
	settled := 0
	stp := store.WithActor(ctx, STPActor)
	for _, run := range runs {
		if run.Items, err = store.Select[domain.ReconciliationItem](q, "SELECT * FROM reconciliation_items WHERE run_id = ? ORDER BY id", run.ID); err != nil {
			return settled, err
		}
		if run.OpenExceptionCount() > 0 {
			continue
		}
		if _, err := s.Settle(stp, run.ID); err != nil {
			if code := domain.CodeOf(err); code == "nothing_to_settle" || code == "run_already_settled" {
				continue
			}
			return settled, err
		}
		settled++
	}
	return settled, nil
}

// settlementFile is a provider's daily report in the inbox: <provider>_<YYYYMMDD>.csv. Anything
// else, including partial uploads (.part, .tmp), is ignored.
var settlementFile = regexp.MustCompile(`^([a-z0-9-]+)_(\d{8})\.csv$`)

// ImportInbox reconciles every settlement report waiting in dir, where SFTP or object-storage
// sync delivers them. Each file then moves to processed/ or rejected/, so it is read only once.
// A file is rejected when the provider is unknown, it does not parse, or that provider and day
// are already reconciled. Infrastructure errors leave the file in place for the next attempt.
func (s *Reconciliation) ImportInbox(ctx context.Context, dir string) (imported, rejected int, err error) {
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
		m := settlementFile.FindStringSubmatch(entry.Name())
		if entry.IsDir() || m == nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		reason, err := s.importFile(stp, path, m[1], m[2])
		if err != nil {
			return imported, rejected, err
		}
		target := "processed"
		if reason != "" {
			target = "rejected"
			rejected++
			s.a.Logger.Warn("settlement report rejected", "file", entry.Name(), "reason", reason)
			if err := s.a.db.Atomic(stp, func(tx *store.Tx) error {
				return tx.Audit("reconciliation.file_rejected", "settlement_file", entry.Name(), map[string]any{"reason": reason})
			}); err != nil {
				return imported, rejected, err
			}
		} else {
			imported++
		}
		if err := os.Rename(path, s.archivePath(dir, target, entry.Name())); err != nil {
			return imported, rejected, err
		}
	}
	return imported, rejected, nil
}

// importFile reconciles one report; a non-empty reason means the file was refused.
func (s *Reconciliation) importFile(ctx context.Context, path, provider, day string) (string, error) {
	if !s.a.providerNamed(provider) {
		return "unknown provider " + provider, nil
	}
	date, err := time.Parse("20060102", day)
	if err != nil {
		return "invalid date " + day, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is a file listed from the operator-configured inbox
	if err != nil {
		return "", err
	}
	report, err := ParseSettlementCSV(string(raw))
	if err == nil {
		_, err = s.Run(ctx, provider, domain.DateOf(date), report)
	}
	if refusal, ok := domain.AsError(err); ok {
		return refusal.Message, nil
	}
	return "", err
}

// archivePath is where a handled report is filed, never overwriting an earlier file of that name.
func (s *Reconciliation) archivePath(dir, target, name string) string {
	path := filepath.Join(dir, target, name)
	if _, err := os.Stat(path); err == nil {
		path += "." + s.a.now().Format("20060102T150405.000000")
	}
	return path
}

func derefInt(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
