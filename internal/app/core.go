package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ---------------------------------------------------------------------------------------------
// EB — business dates (T24: DATES) and holiday calendar

type Dates struct{ a *App }

func (d *Dates) Get(q store.Q) (*domain.SystemDates, error) {
	dates, err := store.Get[domain.SystemDates](q, "SELECT * FROM system_dates LIMIT 1")
	if err == nil && dates == nil {
		err = errors.New("business dates are not initialised: bootstrap the bank first")
	}
	return dates, err
}

// OnlineBookingDate is today, or the next working day while COB runs.
func (d *Dates) OnlineBookingDate(q store.Q) (domain.Date, error) {
	dates, err := d.Get(q)
	if err != nil {
		return domain.Date{}, err
	}
	return dates.OnlineBookingDate(), nil
}

// businessDaysAfter is the business date n working days after today's online booking date.
func (d *Dates) businessDaysAfter(q store.Q, n int) (domain.Date, error) {
	day, err := d.OnlineBookingDate(q)
	if err != nil {
		return domain.Date{}, err
	}
	holidays, err := d.Holidays(q)
	if err != nil {
		return domain.Date{}, err
	}
	for range n {
		day = domain.NextWorkingDayAfter(day, holidays)
	}
	return day, nil
}

func (d *Dates) Holidays(q store.Q) (domain.Holidays, error) {
	rows, err := store.Select[domain.Holiday](q, "SELECT * FROM holidays WHERE country = ?", d.a.Options.Country)
	if err != nil {
		return nil, err
	}
	set := domain.Holidays{}
	for _, h := range rows {
		set[h.Date] = true
	}
	return set, nil
}

// Numbers allocates customer and account numbers from concurrency-safe sequences.
type Numbers struct{ a *App }

const (
	CustomerSequence = "customer"
	AccountSequence  = "account"
)

func (n *Numbers) Next(tx *store.Tx, name string) (int64, error) {
	if !tx.Serial() { // from a block, so parallel openings don't queue on the sequence row
		return n.a.sequences.take("number:"+name, func(size int64) (start int64, err error) {
			err = tx.Store().Independent(tx.Context(), func(q store.Q) error {
				start, err = reserveNumbers(q, name, size)
				return err
			})
			return start, err
		})
	}
	seq, err := store.Get[domain.NumberSequence](tx, "SELECT * FROM number_sequences WHERE name = ?", name)
	if err != nil {
		return 0, err
	}
	if seq == nil {
		return 0, fmt.Errorf("number sequence '%s' is not initialised", name)
	}
	value := seq.Take()
	return value, tx.Update("number_sequences", seq)
}

// ---------------------------------------------------------------------------------------------
// EB — parameterised charges (T24: FT.COMMISSION.TYPE) and maker-checker parameter changes

type Charges struct{ a *App }

func (c *Charges) Get(q store.Q, code string) (*domain.ChargeType, error) {
	charge, err := store.Get[domain.ChargeType](q, "SELECT * FROM charge_types WHERE code = ?", code)
	if err == nil && charge == nil {
		err = fmt.Errorf("charge type '%s' is not configured", code)
	}
	return charge, err
}

func (c *Charges) Calculate(q store.Q, code string, amount int64) (int64, error) {
	charge, err := c.Get(q, code)
	if err != nil {
		return 0, err
	}
	return charge.Rule().Calculate(amount), nil
}

type ChargeTypeChange struct {
	Description string `json:"description"`
	BasisPoints int    `json:"basisPoints"`
}

type InterestConditionChange struct {
	Category   int             `json:"category"`
	Currency   string          `json:"currency"`
	AnnualRate decimal.Decimal `json:"annualRate"`
}

// Parameters is maker-checker for parameter tables. A change is captured as an unauthorised
// request; a different operator authorises it, at which point it is applied (and the previous
// version lands in record history).
type Parameters struct{ a *App }

func (p *Parameters) ChargeTypes(ctx context.Context) ([]domain.ChargeType, error) {
	return store.Select[domain.ChargeType](p.a.read(ctx), "SELECT * FROM charge_types ORDER BY code")
}

func (p *Parameters) InterestConditions(ctx context.Context) ([]domain.InterestCondition, error) {
	return store.Select[domain.InterestCondition](p.a.read(ctx), "SELECT * FROM interest_conditions ORDER BY category, currency")
}

func (p *Parameters) Pending(ctx context.Context) ([]domain.ParameterChangeRequest, error) {
	return store.Select[domain.ParameterChangeRequest](p.a.read(ctx),
		"SELECT * FROM parameter_change_requests WHERE status = ? ORDER BY input_at", domain.Unauthorised)
}

func (p *Parameters) ProposeCharge(ctx context.Context, code string, change ChargeTypeChange) (*domain.ParameterChangeRequest, error) {
	code = strings.ToUpper(code)
	if _, err := domain.NewChargeType(code, change.Description, change.BasisPoints); err != nil { // validate at input time
		return nil, err
	}
	return p.propose(ctx, domain.ParamChargeType, code, change)
}

func (p *Parameters) ProposeInterest(ctx context.Context, change InterestConditionChange) (*domain.ParameterChangeRequest, error) {
	change.Currency = strings.ToUpper(change.Currency)
	if _, err := p.a.Chart.GLFor(p.a.read(ctx), change.Category); err != nil {
		return nil, err
	}
	if _, err := domain.NewInterestCondition(change.Category, change.Currency, change.AnnualRate); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamInterestCondition, fmt.Sprintf("%d.%s", change.Category, change.Currency), change)
}

func (p *Parameters) propose(ctx context.Context, typ domain.ParameterType, key string, change any) (*domain.ParameterChangeRequest, error) {
	payload, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	var request *domain.ParameterChangeRequest
	err = p.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if err := tx.Lock("param:" + string(typ) + ":" + key); err != nil { // one pending change per record
			return err
		}
		pending, err := store.Exists(tx, "SELECT 1 FROM parameter_change_requests WHERE type = ? AND param_key = ? AND status = ?", typ, key, domain.Unauthorised)
		if err != nil {
			return err
		}
		if pending {
			return domain.Conflict("change_pending", "An unauthorised change for %s '%s' is already pending.", typ, key)
		}
		request = &domain.ParameterChangeRequest{ID: domain.NewID(), Type: typ, Key: key, Payload: string(payload),
			Status: domain.Unauthorised, Inputter: tx.Actor(), InputAt: tx.Now()}
		if err := tx.Insert("parameter_change_requests", request); err != nil {
			return err
		}
		return tx.Audit("parameter.change_input", string(typ), key, change)
	})
	return request, err
}

// Request returns a change request.
func (p *Parameters) Request(ctx context.Context, id uuid.UUID) (*domain.ParameterChangeRequest, error) {
	r, err := store.Get[domain.ParameterChangeRequest](p.a.read(ctx), "SELECT * FROM parameter_change_requests WHERE id = ?", id)
	if err == nil && r == nil {
		err = domain.NotFound("parameter_change", id)
	}
	return r, err
}

func (p *Parameters) loadRequest(tx *store.Tx, id uuid.UUID) (*domain.ParameterChangeRequest, error) {
	r, err := store.Get[domain.ParameterChangeRequest](tx, "SELECT * FROM parameter_change_requests WHERE id = ?", id)
	if err == nil && r == nil {
		err = domain.NotFound("parameter_change", id)
	}
	return r, err
}

func (p *Parameters) Authorise(ctx context.Context, id uuid.UUID) (*domain.ParameterChangeRequest, error) {
	var request *domain.ParameterChangeRequest
	err := p.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if request, err = p.loadRequest(tx, id); err != nil {
			return err
		}
		if err := request.Authorise(tx.Actor(), tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("parameter_change_requests", request); err != nil {
			return err
		}
		if err := p.apply(tx, request); err != nil {
			return err
		}
		if err := configChanged(tx); err != nil {
			return err
		}
		return tx.Audit("parameter.change_authorised", string(request.Type), request.Key, map[string]any{"id": request.ID, "inputter": request.Inputter})
	})
	if err == nil && request.Type == domain.ParamHookEndpoint {
		err = p.a.Extensions.RefreshCobJobs(ctx)
	}
	if err == nil && (request.Type == domain.ParamHookEndpoint || request.Type == domain.ParamProviderChannel) {
		err = p.a.Providers.Rebuild(ctx)
	}
	if err == nil && (request.Type == domain.ParamLocalTable || request.Type == domain.ParamProviderChannel) {
		// DDL runs after the commit, outside any unit of work; a failure stays queued with its
		// error for an operator (the authorisation itself stands).
		if ddlErr := p.a.LocalTables.ApplyPending(ctx); ddlErr != nil {
			p.a.Logger.Error("local table DDL", "error", ddlErr)
		}
	}
	if table, ok := p.a.Rates.(*TableRates); ok && err == nil && request.Type == domain.ParamCurrencyRates {
		err = table.load(p.a.read(ctx))
	}
	return request, err
}

func (p *Parameters) apply(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	if handled, err := p.applyOperation(tx, request); handled {
		return err
	}
	switch request.Type {
	case domain.ParamOperator:
		return p.a.Operators.apply(tx, request)
	case domain.ParamSuspenseType:
		return p.a.Suspense.applyType(tx, request)
	case domain.ParamHookEndpoint:
		return p.a.Extensions.applyEndpoint(tx, request)
	case domain.ParamCorrespondent:
		return p.a.Nostro.applyCorrespondent(tx, request)
	case domain.ParamNostroAccount:
		return p.a.Nostro.applyNostro(tx, request)
	case domain.ParamProviderChannel:
		return p.a.Channels.apply(tx, request)
	case domain.ParamLocalTable:
		return p.a.LocalTables.apply(tx, request)
	case domain.ParamLocalRecord:
		return p.a.LocalTables.applyRecordRequest(tx, request)
	case domain.ParamTransactionType:
		return p.a.Transactions.applyType(tx, request)
	case domain.ParamVersion:
		return p.a.Transactions.applyVersion(tx, request)
	case domain.ParamEnquiry:
		return p.a.Enquiries.apply(tx, request)
	case domain.ParamOnboardingPolicy:
		return p.a.Customers.applyOnboardingPolicy(tx, request)
	case domain.ParamChargeType:
		var change ChargeTypeChange
		if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
			return err
		}
		charge, err := store.Get[domain.ChargeType](tx, "SELECT * FROM charge_types WHERE code = ?", request.Key)
		if err != nil {
			return err
		}
		if charge == nil {
			created, err := domain.NewChargeType(request.Key, change.Description, change.BasisPoints)
			if err != nil {
				return err
			}
			return tx.Insert("charge_types", created)
		}
		if err := charge.Update(change.Description, change.BasisPoints); err != nil {
			return err
		}
		return tx.Update("charge_types", charge)
	case domain.ParamInterestCondition:
		var change InterestConditionChange
		if err := json.Unmarshal([]byte(request.Payload), &change); err != nil {
			return err
		}
		condition, err := store.Get[domain.InterestCondition](tx, "SELECT * FROM interest_conditions WHERE category = ? AND currency = ?", change.Category, change.Currency)
		if err != nil {
			return err
		}
		if condition == nil {
			created, err := domain.NewInterestCondition(change.Category, change.Currency, change.AnnualRate)
			if err != nil {
				return err
			}
			return tx.Insert("interest_conditions", created)
		}
		if err := condition.Update(change.AnnualRate); err != nil {
			return err
		}
		return tx.Update("interest_conditions", condition)
	}
	return fmt.Errorf("unknown parameter type %s", request.Type)
}

func (p *Parameters) Reject(ctx context.Context, id uuid.UUID, reason string) (*domain.ParameterChangeRequest, error) {
	var request *domain.ParameterChangeRequest
	err := p.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if request, err = p.loadRequest(tx, id); err != nil {
			return err
		}
		if err := request.Reject(tx.Actor(), reason, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("parameter_change_requests", request); err != nil {
			return err
		}
		return tx.Audit("parameter.change_rejected", string(request.Type), request.Key, map[string]any{"id": request.ID, "reason": reason})
	})
	return request, err
}

// ---------------------------------------------------------------------------------------------
// EB — manual GL adjustments under maker-checker

// Adjustments: input creates an INAU record; a different operator must authorise it before it
// is posted. Customer accounts cannot be adjusted this way — customer money only moves through
// product flows.
type Adjustments struct{ a *App }

func (s *Adjustments) Input(ctx context.Context, currency, narrative string, lines []domain.AdjustmentLine) (*domain.ManualAdjustment, error) {
	var adjustment *domain.ManualAdjustment
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		checked := map[string]bool{}
		for _, line := range lines {
			if checked[line.AccountNumber] {
				continue
			}
			checked[line.AccountNumber] = true
			acct, err := store.Get[domain.LedgerAccount](tx, accountColumns+" WHERE account_number = ?", line.AccountNumber)
			if err != nil {
				return err
			}
			if acct == nil {
				return domain.NotFound("account", line.AccountNumber)
			}
			if acct.Category < 10000 {
				return domain.Rule("customer_account_not_allowed", "%s is a customer account; manual adjustments are restricted to internal and P&L accounts.", line.AccountNumber)
			}
			suspense, err := s.a.Suspense.IsSuspenseCategory(tx, acct.Category)
			if err != nil {
				return err
			}
			if suspense {
				// Suspense money must stay explainable item by item: raise or clear an item instead.
				return domain.Rule("use_suspense_clearing", "%s is a suspense account; raise or clear a suspense item instead of adjusting it.", line.AccountNumber)
			}
			if acct.Currency != currency {
				return domain.Rule("currency_mismatch", "%s is %s, adjustment is %s.", line.AccountNumber, acct.Currency, currency)
			}
		}
		var err error
		if adjustment, err = domain.NewManualAdjustment(currency, narrative, lines, tx.Actor(), tx.Now()); err != nil {
			return err
		}
		if err := tx.Insert("manual_adjustments", adjustment); err != nil {
			return err
		}
		return tx.Audit("adjustment.input", "manual_adjustment", adjustment.ID.String(), map[string]any{"currency": currency, "narrative": narrative, "lines": lines})
	})
	return adjustment, err
}

func (s *Adjustments) load(tx *store.Tx, id uuid.UUID) (*domain.ManualAdjustment, error) {
	m, err := store.Get[domain.ManualAdjustment](tx, "SELECT * FROM manual_adjustments WHERE id = ?", id)
	if err == nil && m == nil {
		err = domain.NotFound("manual_adjustment", id)
	}
	return m, err
}

func (s *Adjustments) Authorise(ctx context.Context, id uuid.UUID) (*domain.ManualAdjustment, error) {
	var adjustment *domain.ManualAdjustment
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if adjustment, err = s.load(tx, id); err != nil {
			return err
		}
		if err := adjustment.EnsureUnauthorised(tx.Actor()); err != nil {
			return err
		}
		builder := domain.NewEntry(adjustment.JournalReference(), domain.CatManualAdjustment, adjustment.Narrative)
		for _, line := range adjustment.Lines {
			acct, err := s.a.Ledger.AccountByNumber(tx, line.AccountNumber)
			if err != nil {
				return err
			}
			builder.Add(acct, line.Direction, line.Amount)
		}
		entry, err := s.a.Ledger.Post(tx, builder)
		if err != nil {
			return err
		}
		if err := adjustment.Authorise(tx.Actor(), entry.ID, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("manual_adjustments", adjustment); err != nil {
			return err
		}
		return tx.Audit("adjustment.authorised", "manual_adjustment", id.String(), map[string]any{"transactionReference": entry.TransactionReference, "inputter": adjustment.Inputter})
	})
	return adjustment, err
}

func (s *Adjustments) Reject(ctx context.Context, id uuid.UUID, reason string) (*domain.ManualAdjustment, error) {
	var adjustment *domain.ManualAdjustment
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if adjustment, err = s.load(tx, id); err != nil {
			return err
		}
		if err := adjustment.Reject(tx.Actor(), reason, tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("manual_adjustments", adjustment); err != nil {
			return err
		}
		return tx.Audit("adjustment.rejected", "manual_adjustment", id.String(), map[string]any{"reason": reason})
	})
	return adjustment, err
}

func (s *Adjustments) List(ctx context.Context, status *domain.AuthorisationStatus) ([]domain.ManualAdjustment, error) {
	if status != nil {
		return store.Select[domain.ManualAdjustment](s.a.read(ctx), "SELECT * FROM manual_adjustments WHERE status = ? ORDER BY input_at DESC LIMIT 100", *status)
	}
	return store.Select[domain.ManualAdjustment](s.a.read(ctx), "SELECT * FROM manual_adjustments ORDER BY input_at DESC LIMIT 100")
}
