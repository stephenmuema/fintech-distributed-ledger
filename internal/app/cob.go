package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// CobContext is what a COB job knows about the day being closed.
type CobContext struct {
	RunID            uuid.UUID
	BusinessDate     domain.Date
	NextBusinessDate domain.Date
	IsMonthEnd       bool
}

// AccrualDays is the calendar days covered by today's accrual (a Friday accrues through the weekend).
func (c CobContext) AccrualDays() int { return c.BusinessDate.DaysUntil(c.NextBusinessDate) }

type CobJobResult struct {
	Records int
	Summary string
}

// CobJob is a unit of Close-of-Business work contributed by a module (T24: a BATCH job).
// Implementations must be idempotent per business date: a halted COB is resumed by re-running
// the failed job, which may have partially completed.
type CobJob interface {
	Name() string
	Module() string
	Stage() domain.CobStage
	Run(ctx context.Context, c CobContext) (CobJobResult, error)
}

// NewCobJob builds a job from a function.
func NewCobJob(name, module string, stage domain.CobStage, run func(context.Context, CobContext) (CobJobResult, error)) CobJob {
	return funcJob{name, module, stage, run}
}

type funcJob struct {
	name, module string
	stage        domain.CobStage
	run          func(context.Context, CobContext) (CobJobResult, error)
}

func (j funcJob) Name() string                                                { return j.name }
func (j funcJob) Module() string                                              { return j.module }
func (j funcJob) Stage() domain.CobStage                                      { return j.stage }
func (j funcJob) Run(ctx context.Context, c CobContext) (CobJobResult, error) { return j.run(ctx, c) }

// Cob runs Close of Business:
//  1. enter COB mode (online transactions now book to the next working day);
//  2. run each module's jobs by stage, each committing its own work;
//  3. on failure: halt, leave the system in COB mode and wait for an operator to resume;
//  4. Start of Day: roll the business date and reopen online.
type Cob struct {
	a    *App
	gate sync.Mutex // only one COB executes at a time in this process
}

// cobLease is how long the instance running close of business holds it without renewing; it
// renews it as COB runs, so another instance can resume a COB only once its runner has died.
const cobLease = time.Minute

// Run runs close of business, on one instance at a time (the "cob" lease, ADR 0030).
func (c *Cob) Run(ctx context.Context) (*domain.CobRun, error) {
	if !c.gate.TryLock() {
		return nil, domain.Conflict("cob_in_progress", "Close of Business is already running.")
	}
	defer c.gate.Unlock()
	var run *domain.CobRun
	ran, err := c.a.db.WithLease(ctx, "cob", c.a.Instance, cobLease, func() (err error) {
		run, err = c.run(ctx)
		return err
	})
	if err == nil && !ran {
		return nil, domain.Conflict("cob_in_progress", "Close of Business is already running on another instance.")
	}
	return run, err
}

func (c *Cob) run(ctx context.Context) (*domain.CobRun, error) {
	if err := c.a.Extensions.RefreshCobJobs(ctx); err != nil { // plan COB from current endpoint parameters
		return nil, err
	}

	runID, err := c.startOrResume(ctx)
	if err != nil {
		return nil, err
	}
	run, err := c.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	dates, err := c.a.Dates.Get(c.a.read(ctx))
	if err != nil {
		return nil, err
	}
	cctx := CobContext{run.ID, run.BusinessDate, dates.NextWorkingDay, run.IsMonthEnd}

	for _, job := range run.Jobs {
		if job.Status == domain.JobCompleted {
			continue
		}
		ok, err := c.execute(ctx, run.ID, job.ID, job.Name, cctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return c.Get(ctx, run.ID)
		}
	}
	return c.startOfDay(ctx, run.ID)
}

func (c *Cob) Get(ctx context.Context, id uuid.UUID) (*domain.CobRun, error) {
	return c.load(c.a.read(ctx), id)
}

func (c *Cob) load(q store.Q, id uuid.UUID) (*domain.CobRun, error) {
	run, err := store.Get[domain.CobRun](q, "SELECT * FROM cob_runs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, domain.NotFound("cob_run", id)
	}
	run.Jobs, err = store.Select[domain.CobJobRun](q, "SELECT * FROM cob_job_runs WHERE run_id = ? ORDER BY job_order", id)
	return run, err
}

func (c *Cob) List(ctx context.Context) ([]domain.CobRun, error) {
	q := c.a.read(ctx)
	runs, err := store.Select[domain.CobRun](q, "SELECT * FROM cob_runs ORDER BY started_at DESC LIMIT 30")
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if runs[i].Jobs, err = store.Select[domain.CobJobRun](q, "SELECT * FROM cob_job_runs WHERE run_id = ? ORDER BY job_order", runs[i].ID); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

func (c *Cob) startOrResume(ctx context.Context) (uuid.UUID, error) {
	var runID uuid.UUID
	err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
		dates, err := c.a.Dates.Get(tx)
		if err != nil {
			return err
		}
		if dates.Status == domain.StatusCloseOfBusiness && dates.CurrentCobRunID != nil {
			run, err := store.Get[domain.CobRun](tx, "SELECT * FROM cob_runs WHERE id = ?", *dates.CurrentCobRunID)
			if err != nil {
				return err
			}
			if err := run.Resume(); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE cob_runs SET status = ? WHERE id = ?", run.Status, run.ID); err != nil {
				return err
			}
			runID = run.ID
			c.a.Logger.Warn("resuming halted COB", "run", run.ID, "businessDate", run.BusinessDate)
			return tx.Audit("cob.resumed", "cob_run", run.ID.String(), map[string]any{"businessDate": run.BusinessDate})
		}

		jobs := c.a.CobJobs()
		sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Stage().Order() < jobs[j].Stage().Order() })
		specs := make([]domain.JobSpec, len(jobs))
		for i, j := range jobs {
			specs[i] = domain.JobSpec{Name: j.Name(), Stage: j.Stage()}
		}
		run := domain.NewCobRun(dates.Today, dates.IsMonthEnd(), specs, tx.Actor(), tx.Now())
		if err := tx.Insert("cob_runs", run); err != nil {
			return err
		}
		for i := range run.Jobs {
			if err := tx.Insert("cob_job_runs", &run.Jobs[i]); err != nil {
				return err
			}
		}
		if err := dates.EnterCob(run.ID); err != nil {
			return err
		}
		if err := tx.Update("system_dates", dates); err != nil {
			return err
		}
		runID = run.ID
		c.a.Logger.Info("COB started", "businessDate", run.BusinessDate, "monthEnd", run.IsMonthEnd)
		return tx.Audit("cob.started", "cob_run", run.ID.String(), map[string]any{"businessDate": run.BusinessDate, "isMonthEnd": run.IsMonthEnd})
	})
	if err != nil {
		return runID, err
	}
	// A posting that read "online, today" before the switch books to today: let it commit before
	// the end-of-day jobs take today's balances, on this instance and every other (ADR 0030).
	// Postings that start now book to the next day.
	return runID, c.a.awaitInstances(ctx, runID)
}

func (c *Cob) updateJob(ctx context.Context, jobID uuid.UUID, change func(tx *store.Tx, j *domain.CobJobRun) error) error {
	return c.a.db.Atomic(ctx, func(tx *store.Tx) error {
		job, err := store.Get[domain.CobJobRun](tx, "SELECT * FROM cob_job_runs WHERE id = ?", jobID)
		if err != nil {
			return err
		}
		if err := change(tx, job); err != nil {
			return err
		}
		return tx.Update("cob_job_runs", job)
	})
}

// execute runs one job; it reports false (and halts the run) when the job fails.
func (c *Cob) execute(ctx context.Context, runID, jobRunID uuid.UUID, name string, cctx CobContext) (bool, error) {
	if err := c.updateJob(ctx, jobRunID, func(tx *store.Tx, j *domain.CobJobRun) error { j.Start(tx.Now()); return nil }); err != nil {
		return false, err
	}

	result, jobErr := c.runJob(ctx, name, cctx)
	if jobErr == nil {
		c.a.Logger.Info("COB job completed", "job", name, "records", result.Records, "summary", result.Summary)
		return true, c.updateJob(ctx, jobRunID, func(tx *store.Tx, j *domain.CobJobRun) error {
			j.Succeed(result.Records, optional(result.Summary), tx.Now())
			return nil
		})
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	c.a.Logger.Error("COB job failed — COB halted", "job", name, "error", jobErr)
	return false, c.updateJob(ctx, jobRunID, func(tx *store.Tx, j *domain.CobJobRun) error {
		j.Fail(jobErr.Error(), tx.Now())
		if _, err := tx.Exec("UPDATE cob_runs SET status = ? WHERE id = ?", domain.CobHalted, runID); err != nil {
			return err
		}
		return tx.Audit("cob.halted", "cob_run", runID.String(), map[string]any{"job": name, "error": jobErr.Error()})
	})
}

func (c *Cob) runJob(ctx context.Context, name string, cctx CobContext) (result CobJobResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("job panicked: %v", p)
		}
	}()
	for _, job := range c.a.CobJobs() {
		if job.Name() == name {
			return job.Run(ctx, cctx)
		}
	}
	return CobJobResult{}, fmt.Errorf("COB job %s is not registered", name)
}

func (c *Cob) startOfDay(ctx context.Context, runID uuid.UUID) (*domain.CobRun, error) {
	err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
		holidays, err := c.a.Dates.Holidays(tx)
		if err != nil {
			return err
		}
		dates, err := c.a.Dates.Get(tx)
		if err != nil {
			return err
		}
		run, err := c.load(tx, runID)
		if err != nil {
			return err
		}
		var report *string
		for _, j := range run.Jobs {
			if j.Name == "RE.EOD.REPORT" {
				report = j.Summary
			}
		}
		dates.RollToNextWorkingDay(holidays)
		run.Complete(dates.Today, report, tx.Now())
		if err := tx.Update("system_dates", dates); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE cob_runs SET status = ?, new_business_date = ?, report = ?, completed_at = ? WHERE id = ?",
			run.Status, run.NewBusinessDate, run.Report, run.CompletedAt, run.ID); err != nil {
			return err
		}
		c.a.Logger.Info("COB completed", "today", dates.Today)
		return tx.Audit("cob.completed", "cob_run", run.ID.String(), map[string]any{"closed": run.BusinessDate, "today": dates.Today})
	})
	if err != nil {
		return nil, err
	}
	return c.Get(ctx, runID)
}

// ---------------------------------------------------------------------------------------------
// Jobs

// expireCardHoldsJob (CM) releases card authorisation holds the network never captured.
func (c *Cob) expireCardHoldsJob() CobJob {
	return NewCobJob("CM.EXPIRE.HOLDS", "CM", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		n, err := c.a.Cards.ExpireStaleAuthorizations(ctx)
		return CobJobResult{Records: n}, err
	})
}

// expirePaymentLinksJob (PP) deactivates payment links past their expiry.
func (c *Cob) expirePaymentLinksJob() CobJob {
	return NewCobJob("PP.EXPIRE.LINKS", "PP", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		var n int64
		err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
			res, err := tx.Exec("UPDATE payment_links SET is_active = 0, version = version + 1 WHERE is_active = 1 AND expires_at IS NOT NULL AND expires_at <= ?", tx.Now())
			if err == nil {
				n, err = res.RowsAffected()
			}
			return err
		})
		return CobJobResult{Records: int(n)}, err
	})
}

// interestAccrualJob (AC) is the daily credit-interest accrual (T24: AC.INTEREST.ACCRUAL). For
// each account whose category and currency have an interest condition, accrue actual/365 on the
// day's closing balance into the sub-ledger, and post one GL entry per currency:
// Dr interest expense / Cr interest payable.
func (c *Cob) interestAccrualJob() CobJob {
	return NewCobJob("AC.INTEREST.ACCRUAL", "AC", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		conditions, err := store.Select[domain.InterestCondition](c.a.read(ctx), "SELECT * FROM interest_conditions ORDER BY currency, category")
		if err != nil {
			return CobJobResult{}, err
		}
		rates := map[string]map[int]decimal.Decimal{}
		var currencies []string
		for _, cond := range conditions {
			if !cond.AnnualRate.IsPositive() {
				continue
			}
			if rates[cond.Currency] == nil {
				rates[cond.Currency] = map[int]decimal.Decimal{}
				currencies = append(currencies, cond.Currency)
			}
			rates[cond.Currency][cond.Category] = cond.AnnualRate
		}

		accrued := 0
		days := cc.AccrualDays()
		for _, currency := range currencies {
			reference := fmt.Sprintf("cob:interest-accrual:%s:%s", cc.BusinessDate.Format("20060102"), currency)
			err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
				done, err := store.Exists(tx, "SELECT 1 FROM journal_entries WHERE reference = ?", reference)
				if err != nil || done { // already done (resumed COB)
					return err
				}
				categories := make([]int, 0, len(rates[currency]))
				for cat := range rates[currency] {
					categories = append(categories, cat)
				}
				marks, args := store.In(categories)
				wallets, err := store.Select[domain.Wallet](tx, "SELECT * FROM wallets WHERE currency = ? AND category IN ("+marks+") ORDER BY created_at",
					append([]any{currency}, args...)...)
				if err != nil {
					return err
				}
				closing, err := c.closingBalances(tx, cc.BusinessDate)
				if err != nil {
					return err
				}
				var accruals []domain.InterestAccrual
				var total int64
				for _, w := range wallets {
					balance, rate := closing[w.AvailableAccountID], rates[currency][w.Category]
					amount := domain.CalculateInterest(balance, rate, days)
					if amount <= 0 {
						continue
					}
					accruals = append(accruals, domain.InterestAccrual{ID: domain.NewID(), WalletID: w.ID, BusinessDate: cc.BusinessDate,
						Currency: currency, Balance: balance, AnnualRate: rate, Days: days, Amount: amount})
					total += amount
				}
				if len(accruals) == 0 {
					return nil
				}
				expense, err := c.a.Ledger.SystemAccount(tx, domain.InterestExpense(currency))
				if err != nil {
					return err
				}
				payable, err := c.a.Ledger.SystemAccount(tx, domain.InterestPayable(currency))
				if err != nil {
					return err
				}
				description := fmt.Sprintf("Credit interest accrual %s (%dd)", cc.BusinessDate, days)
				if _, err := c.a.Ledger.Post(tx, domain.NewEntry(reference, domain.CatInterestAccrual, description).Debit(expense, total).Credit(payable, total),
					PostOptions{BookingDate: &cc.BusinessDate}); err != nil {
					return err
				}
				for i := range accruals {
					if err := tx.Insert("interest_accruals", &accruals[i]); err != nil {
						return err
					}
				}
				accrued += len(accruals)
				return nil
			})
			if err != nil {
				return CobJobResult{}, err
			}
		}
		return CobJobResult{accrued, fmt.Sprintf("%d accounts accrued over %d day(s)", accrued, days)}, nil
	})
}

// interestCapitalisationJob (AC) is month-end capitalisation (T24: interest liquidation): it
// moves accrued interest from interest payable onto each customer's account.
func (c *Cob) interestCapitalisationJob() CobJob {
	return NewCobJob("AC.INTEREST.CAPITALISE", "AC", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		if !cc.IsMonthEnd {
			return CobJobResult{0, "not month end"}, nil
		}
		currencies, err := store.Select[struct {
			Currency string `db:"currency"`
		}](c.a.read(ctx), "SELECT DISTINCT currency FROM interest_accruals WHERE capitalised = 0 ORDER BY currency")
		if err != nil {
			return CobJobResult{}, err
		}
		capitalised := 0
		for _, row := range currencies {
			currency := row.Currency
			err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
				accruals, err := store.Select[domain.InterestAccrual](tx,
					"SELECT * FROM interest_accruals WHERE capitalised = 0 AND currency = ? AND business_date <= ? ORDER BY id", currency, cc.BusinessDate)
				if err != nil || len(accruals) == 0 {
					return err
				}
				payable, err := c.a.Ledger.SystemAccount(tx, domain.InterestPayable(currency))
				if err != nil {
					return err
				}
				builder := domain.NewEntry(fmt.Sprintf("cob:interest-capitalise:%s:%s", cc.BusinessDate.Format("200601"), currency),
					domain.CatInterestCapitalisation, "Credit interest for "+cc.BusinessDate.Format("January 2006"))

				var total int64
				perWallet := map[uuid.UUID]int64{}
				var order []uuid.UUID
				for _, a := range accruals {
					total += a.Amount
					if _, seen := perWallet[a.WalletID]; !seen {
						order = append(order, a.WalletID)
					}
					perWallet[a.WalletID] += a.Amount
				}
				builder.Debit(payable, total)
				for _, walletID := range order {
					wallet, err := c.a.Accounts.loadWallet(tx, walletID)
					if err != nil {
						return err
					}
					available, err := c.a.Ledger.Account(tx, wallet.AvailableAccountID)
					if err != nil {
						return err
					}
					builder.Credit(available, perWallet[walletID])
				}
				entry, err := c.a.Ledger.Post(tx, builder, PostOptions{BookingDate: &cc.BusinessDate})
				if err != nil {
					return err
				}
				ids := make([]uuid.UUID, len(accruals))
				for i, a := range accruals {
					ids[i] = a.ID
				}
				marks, args := store.In(ids)
				if _, err := tx.Exec("UPDATE interest_accruals SET capitalised = 1, capitalisation_entry_id = ? WHERE id IN ("+marks+")",
					append([]any{entry.ID}, args...)...); err != nil {
					return err
				}
				capitalised += len(order)
				return nil
			})
			if err != nil {
				return CobJobResult{}, err
			}
		}
		return CobJobResult{capitalised, fmt.Sprintf("%d accounts credited with interest", capitalised)}, nil
	})
}

// eodBalancesJob (EB) snapshots the end-of-day balance of every ledger account (T24: ACCT.ACTIVITY).
func (c *Cob) eodBalancesJob() CobJob {
	return NewCobJob("EB.EOD.BALANCES", "EB", domain.StageSystemWide, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		written := 0
		err := c.a.db.Atomic(ctx, func(tx *store.Tx) error {
			date := cc.BusinessDate
			if _, err := tx.Exec("DELETE FROM daily_balances WHERE business_date = ?", date); err != nil { // idempotent re-run
				return err
			}
			closing, err := c.closingBalances(tx, date)
			if err != nil {
				return err
			}
			activity, err := store.Select[struct {
				AccountID uuid.UUID `db:"account_id"`
				Debits    int64     `db:"debits"`
				Credits   int64     `db:"credits"`
			}](tx, `SELECT account_id,
			            SUM(CASE direction WHEN 'Debit' THEN amount ELSE 0 END) AS debits,
			            SUM(CASE direction WHEN 'Credit' THEN amount ELSE 0 END) AS credits
			        FROM journal_lines WHERE booking_date = ? GROUP BY account_id`, date)
			if err != nil {
				return err
			}
			day := map[uuid.UUID][2]int64{}
			for _, a := range activity {
				day[a.AccountID] = [2]int64{a.Debits, a.Credits}
			}
			accounts, err := store.Select[struct {
				ID       uuid.UUID          `db:"id"`
				Currency string             `db:"currency"`
				Type     domain.AccountType `db:"type"`
			}](tx, "SELECT id, currency, type FROM ledger_accounts ORDER BY id")
			if err != nil {
				return err
			}
			for _, a := range accounts {
				debits, credits := day[a.ID][0], day[a.ID][1]
				close := closing[a.ID]
				if close == 0 && debits == 0 && credits == 0 {
					continue
				}
				sign := int64(-1)
				if a.Type.IsDebitNormal() {
					sign = 1
				}
				if err := tx.Insert("daily_balances", &domain.DailyBalance{AccountID: a.ID, BusinessDate: date, Currency: a.Currency,
					Opening: close - sign*(debits-credits), Debits: debits, Credits: credits, Closing: close}); err != nil {
					return err
				}
				written++
			}
			return nil
		})
		return CobJobResult{Records: written}, err
	})
}

// trialBalanceJob (RE): the GL must balance per currency, or COB halts.
func (c *Cob) trialBalanceJob() CobJob {
	return NewCobJob("RE.TRIAL.BALANCE", "RE", domain.StageReporting, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		rows, err := store.Select[struct {
			Currency string             `db:"currency"`
			Type     domain.AccountType `db:"type"`
			Closing  int64              `db:"closing"`
			Count    int                `db:"count"`
		}](c.a.read(ctx), `SELECT b.currency, a.type, SUM(b.closing) AS closing, COUNT(*) AS count
		    FROM daily_balances b JOIN ledger_accounts a ON a.id = b.account_id
		    WHERE b.business_date = ? GROUP BY b.currency, a.type`, cc.BusinessDate)
		if err != nil {
			return CobJobResult{}, err
		}
		net, count := map[string]int64{}, 0
		for _, r := range rows {
			count += r.Count
			if r.Type.IsDebitNormal() {
				net[r.Currency] += r.Closing
			} else {
				net[r.Currency] -= r.Closing
			}
		}
		var breaks []string
		for currency, n := range net {
			if n != 0 {
				breaks = append(breaks, fmt.Sprintf("%s %d", currency, n))
			}
		}
		if len(breaks) > 0 {
			sort.Strings(breaks)
			return CobJobResult{}, fmt.Errorf("trial balance out of balance: %s", strings.Join(breaks, ", "))
		}
		return CobJobResult{count, "debits = credits in every currency"}, nil
	})
}

// ledgerIntegrityJob (RE): every account's stored balance must equal the sum of its journal lines.
func (c *Cob) ledgerIntegrityJob() CobJob {
	return NewCobJob("RE.LEDGER.INTEGRITY", "RE", domain.StageReporting, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		drift, err := c.a.Reports.Integrity(ctx)
		if err != nil {
			return CobJobResult{}, err
		}
		if len(drift) > 0 {
			var numbers []string
			for i, d := range drift {
				if i == 5 {
					break
				}
				numbers = append(numbers, d.AccountNumber)
			}
			return CobJobResult{}, fmt.Errorf("ledger drift on %d account(s): %s", len(drift), strings.Join(numbers, ", "))
		}
		return CobJobResult{0, "no drift"}, nil
	})
}

// eodReportJob (RE) builds the end-of-day report (volumes by category, suspense warnings), stored on the COB run.
func (c *Cob) eodReportJob() CobJob {
	return NewCobJob("RE.EOD.REPORT", "RE", domain.StageReporting, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		q := c.a.read(ctx)
		volumes, err := store.Select[struct {
			Category domain.EntryCategory `db:"category"`
			Count    int                  `db:"count"`
		}](q, "SELECT category, COUNT(*) AS count FROM journal_entries WHERE booking_date = ? GROUP BY category", cc.BusinessDate)
		if err != nil {
			return CobJobResult{}, err
		}
		uncleared, err := c.a.Suspense.ageing(q, &cc.BusinessDate)
		if err != nil {
			return CobJobResult{}, err
		}
		// Deposits nobody could match for a week need a person: a refund to the payer or a tracing call.
		type aged struct {
			Currency string `db:"currency" json:"currency"`
			Count    int    `db:"count" json:"count"`
			Amount   int64  `db:"amount" json:"amount"`
		}
		stale, err := store.Select[aged](q, "SELECT currency, COUNT(*) AS count, SUM(amount) AS amount FROM deposits WHERE status = ? AND received_at < ? GROUP BY currency ORDER BY currency",
			domain.DepositUnapplied, c.a.now().Add(-7*24*time.Hour))
		if err != nil {
			return CobJobResult{}, err
		}
		// Customer accounts below zero (merchants overdrawn by chargebacks) are money we are
		// owed and may never get back: recover from later collections or chase the merchant.
		overdrawn, err := store.Select[struct {
			AccountNumber string `db:"account_number" json:"accountNumber"`
			Currency      string `db:"currency" json:"currency"`
			Balance       int64  `db:"balance" json:"balance"`
		}](q, "SELECT account_number, currency, balance FROM ledger_accounts WHERE category < 10000 AND balance < 0 ORDER BY balance LIMIT 500")
		if err != nil {
			return CobJobResult{}, err
		}
		entries, total := map[string]int{}, 0
		for _, v := range volumes {
			entries[string(v.Category)] = v.Count
			total += v.Count
		}
		report, err := json.Marshal(map[string]any{
			"business_date": cc.BusinessDate, "month_end": cc.IsMonthEnd, "entries": entries, "uncleared_suspense": uncleared,
			"unapplied_deposits_over_7_days": stale, "negative_customer_balances": overdrawn,
		})
		return CobJobResult{total, string(report)}, err
	})
}

// closingBalances returns every account's balance as at the end of date, unwinding postings
// booked after it (e.g. online activity during COB).
func (c *Cob) closingBalances(q store.Q, date domain.Date) (map[uuid.UUID]int64, error) {
	rows, err := store.Select[struct {
		ID      uuid.UUID          `db:"id"`
		Type    domain.AccountType `db:"type"`
		Balance int64              `db:"balance"`
		Later   int64              `db:"later"`
	}](q, `SELECT a.id, a.type, a.balance, COALESCE(l.net, 0) AS later
	    FROM ledger_accounts a
	    LEFT JOIN (SELECT account_id, SUM(CASE direction WHEN 'Debit' THEN amount ELSE -amount END) AS net
	               FROM journal_lines WHERE booking_date > ? GROUP BY account_id) l ON l.account_id = a.id`, date)
	if err != nil {
		return nil, err
	}
	closing := make(map[uuid.UUID]int64, len(rows))
	for _, r := range rows {
		sign := int64(-1)
		if r.Type.IsDebitNormal() {
			sign = 1
		}
		closing[r.ID] = r.Balance - sign*r.Later
	}
	return closing, nil
}

// standingOrdersJob (FT) pays standing orders due on or before the next working day. During
// COB postings book to that day, so a payment due on a weekend or holiday goes out, value dated,
// on the following working day.
func (c *Cob) standingOrdersJob() CobJob {
	return NewCobJob("FT.STANDING.ORDERS", "FT", domain.StageApplication, func(ctx context.Context, cc CobContext) (CobJobResult, error) {
		completed, declined, err := c.a.StandingOrders.Execute(ctx, cc.NextBusinessDate)
		return CobJobResult{completed + declined, fmt.Sprintf("%d payment(s) made, %d declined", completed, declined)}, err
	})
}

// expireDisputesJob (PP) loses disputes whose evidence window closed without evidence.
func (c *Cob) expireDisputesJob() CobJob {
	return NewCobJob("PP.DISPUTES.EXPIRE", "PP", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		n, err := c.a.Disputes.ExpireOverdue(ctx)
		return CobJobResult{n, fmt.Sprintf("%d dispute(s) lost without evidence", n)}, err
	})
}

// autoSettleJob (RC) reconciles the settlement reports waiting in the inbox, then settles every
// run with no open breaks, including runs whose breaks operators resolved during the day.
func (c *Cob) autoSettleJob() CobJob {
	return NewCobJob("RC.AUTO.SETTLE", "RC", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		imported, rejected := 0, 0
		if inbox := c.a.Options.SettlementInbox; inbox != "" {
			var err error
			if imported, rejected, err = c.a.Recon.ImportInbox(ctx, inbox); err != nil {
				return CobJobResult{}, err
			}
		}
		settled, err := c.a.Recon.SettleClean(ctx)
		return CobJobResult{imported + settled, fmt.Sprintf("%d report(s) reconciled, %d rejected, %d run(s) settled", imported, rejected, settled)}, err
	})
}

// depositAutoMatchJob (AC) credits unapplied deposits whose reference now matches an account,
// before interest accrual so the money counts from today.
func (c *Cob) depositAutoMatchJob() CobJob {
	return NewCobJob("AC.DEPOSITS.AUTOMATCH", "AC", domain.StageApplication, func(ctx context.Context, _ CobContext) (CobJobResult, error) {
		n, err := c.a.Deposits.AutoMatch(ctx)
		return CobJobResult{n, fmt.Sprintf("%d unapplied deposit(s) matched to accounts", n)}, err
	})
}
