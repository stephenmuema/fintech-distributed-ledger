package domain

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Historised marks records whose previous versions are kept in record history (T24: $HIS files).
type Historised interface {
	HistoryName() string
}

// ---------------------------------------------------------------------------------------------
// Business dates (T24: DATES) and holidays

type SystemStatus string

const (
	StatusOnline          SystemStatus = "Online"
	StatusCloseOfBusiness SystemStatus = "CloseOfBusiness"
)

// Holiday is a non-working day for the bank's country (T24: HOLIDAY).
type Holiday struct {
	Date    Date   `db:"holiday_date" json:"date"`
	Country string `db:"country" json:"country"`
	Name    string `db:"name" json:"name"`
}

// Holidays is a set of non-working dates.
type Holidays map[Date]bool

// SystemDates holds the bank's business dates. The business date is deliberately decoupled from
// the wall clock: it only moves when Close of Business completes. While COB runs, the system
// stays online but books new transactions to the next working day.
type SystemDates struct {
	ID              uuid.UUID    `db:"id" json:"id"`
	Version         int64        `db:"version" json:"version"`
	Today           Date         `db:"today" json:"today"`
	LastWorkingDay  Date         `db:"last_working_day" json:"lastWorkingDay"`
	NextWorkingDay  Date         `db:"next_working_day" json:"nextWorkingDay"`
	Status          SystemStatus `db:"status" json:"status"`
	CurrentCobRunID *uuid.UUID   `db:"current_cob_run_id" json:"currentCobRunId"`
}

func InitialiseSystemDates(calendarDate Date, holidays Holidays) *SystemDates {
	today := calendarDate
	if !IsWorkingDay(today, holidays) {
		today = NextWorkingDayAfter(calendarDate, holidays)
	}
	return &SystemDates{
		ID:             NewID(),
		Today:          today,
		LastWorkingDay: PreviousWorkingDayBefore(today, holidays),
		NextWorkingDay: NextWorkingDayAfter(today, holidays),
		Status:         StatusOnline,
	}
}

// OnlineBookingDate is the booking date for an online transaction right now.
func (d *SystemDates) OnlineBookingDate() Date {
	if d.Status == StatusOnline {
		return d.Today
	}
	return d.NextWorkingDay
}

// IsMonthEnd is true when closing Today crosses into a new month (month-end processing).
func (d *SystemDates) IsMonthEnd() bool {
	return d.NextWorkingDay.M != d.Today.M || d.NextWorkingDay.Y != d.Today.Y
}

func (d *SystemDates) EnterCob(runID uuid.UUID) error {
	if d.Status == StatusCloseOfBusiness && (d.CurrentCobRunID == nil || *d.CurrentCobRunID != runID) {
		return Conflict("cob_in_progress", "Close of Business is already running.")
	}
	d.Status = StatusCloseOfBusiness
	d.CurrentCobRunID = &runID
	return nil
}

// RollToNextWorkingDay is Start of Day: roll the business date forward and reopen online.
func (d *SystemDates) RollToNextWorkingDay(holidays Holidays) {
	d.LastWorkingDay = d.Today
	d.Today = d.NextWorkingDay
	d.NextWorkingDay = NextWorkingDayAfter(d.Today, holidays)
	d.Status = StatusOnline
	d.CurrentCobRunID = nil
}

func (d SystemDates) MarshalJSON() ([]byte, error) {
	type alias SystemDates
	return json.Marshal(struct {
		alias
		OnlineBookingDate Date `json:"onlineBookingDate"`
		IsMonthEnd        bool `json:"isMonthEnd"`
	}{alias(d), d.OnlineBookingDate(), d.IsMonthEnd()})
}

func IsWorkingDay(date Date, holidays Holidays) bool {
	wd := date.Weekday()
	return wd != time.Saturday && wd != time.Sunday && !holidays[date]
}

func NextWorkingDayAfter(from Date, holidays Holidays) Date {
	d := from.AddDays(1)
	for !IsWorkingDay(d, holidays) {
		d = d.AddDays(1)
	}
	return d
}

func PreviousWorkingDayBefore(from Date, holidays Holidays) Date {
	d := from.AddDays(-1)
	for !IsWorkingDay(d, holidays) {
		d = d.AddDays(-1)
	}
	return d
}

// NumberSequence is a gap-free, concurrency-safe number generator (customer and account numbers).
type NumberSequence struct {
	ID        uuid.UUID `db:"id" json:"id"`
	Version   int64     `db:"version" json:"version"`
	Name      string    `db:"name" json:"name"`
	NextValue int64     `db:"next_value" json:"nextValue"`
}

func (s *NumberSequence) Take() int64 {
	v := s.NextValue
	s.NextValue++
	return v
}

// ---------------------------------------------------------------------------------------------
// Close of Business

// CobStage is a COB stage; stages run in Order (mirrors T24 batch stages).
type CobStage string

const (
	// StageApplication is application-level processing: hold expiry, accruals, capitalisation.
	StageApplication CobStage = "Application"
	// StageSystemWide is end-of-day balance snapshots.
	StageSystemWide CobStage = "SystemWide"
	// StageReporting is trial balance, integrity checks and the EOD report.
	StageReporting CobStage = "Reporting"
)

func (s CobStage) Order() int {
	switch s {
	case StageApplication:
		return 1
	case StageSystemWide:
		return 2
	}
	return 3
}

type CobRunStatus string

const (
	CobRunning   CobRunStatus = "Running"
	CobHalted    CobRunStatus = "Halted"
	CobCompleted CobRunStatus = "Completed"
)

type CobJobStatus string

const (
	JobPending   CobJobStatus = "Pending"
	JobCompleted CobJobStatus = "Completed"
	JobFailed    CobJobStatus = "Failed"
)

// CobRun is one Close-of-Business execution for a business date. If a job fails the run halts
// with the system still in COB mode; resuming re-runs from the first incomplete job. Every job
// is idempotent per business date, so a partial run can always be safely resumed.
type CobRun struct {
	ID              uuid.UUID    `db:"id" json:"id"`
	BusinessDate    Date         `db:"business_date" json:"businessDate"`
	IsMonthEnd      bool         `db:"is_month_end" json:"isMonthEnd"`
	Status          CobRunStatus `db:"status" json:"status"`
	StartedBy       string       `db:"started_by" json:"startedBy"`
	StartedAt       time.Time    `db:"started_at" json:"startedAt"`
	CompletedAt     *time.Time   `db:"completed_at" json:"completedAt"`
	NewBusinessDate *Date        `db:"new_business_date" json:"newBusinessDate"`
	Report          *string      `db:"report" json:"report"`
	Jobs            []CobJobRun  `db:"-" json:"jobs"`
}

// JobSpec names a job and its stage when a run is planned.
type JobSpec struct {
	Name  string
	Stage CobStage
}

func NewCobRun(businessDate Date, isMonthEnd bool, jobs []JobSpec, startedBy string, now time.Time) *CobRun {
	run := &CobRun{ID: NewID(), BusinessDate: businessDate, IsMonthEnd: isMonthEnd, Status: CobRunning, StartedBy: startedBy, StartedAt: now}
	for i, j := range jobs {
		run.Jobs = append(run.Jobs, CobJobRun{ID: NewID(), RunID: run.ID, Name: j.Name, Stage: j.Stage, Order: i + 1, Status: JobPending})
	}
	return run
}

// Resume restarts a halted run (or one interrupted by a crash, which is still marked Running).
func (r *CobRun) Resume() error {
	if r.Status == CobCompleted {
		return Conflict("cob_not_halted", "COB run is %s; only halted runs can be resumed.", r.Status)
	}
	r.Status = CobRunning
	return nil
}

func (r *CobRun) Halt() { r.Status = CobHalted }

func (r *CobRun) Complete(newBusinessDate Date, report *string, now time.Time) {
	r.Status = CobCompleted
	r.NewBusinessDate = &newBusinessDate
	r.Report = report
	r.CompletedAt = &now
}

type CobJobRun struct {
	ID               uuid.UUID    `db:"id" json:"id"`
	RunID            uuid.UUID    `db:"run_id" json:"runId"`
	Name             string       `db:"name" json:"name"`
	Stage            CobStage     `db:"stage" json:"stage"`
	Order            int          `db:"job_order" json:"order"`
	Status           CobJobStatus `db:"status" json:"status"`
	Attempts         int          `db:"attempts" json:"attempts"`
	RecordsProcessed int          `db:"records_processed" json:"recordsProcessed"`
	Summary          *string      `db:"summary" json:"summary"`
	Error            *string      `db:"error" json:"error"`
	StartedAt        *time.Time   `db:"started_at" json:"startedAt"`
	FinishedAt       *time.Time   `db:"finished_at" json:"finishedAt"`
}

func (j *CobJobRun) Start(now time.Time) {
	j.Attempts++
	j.StartedAt = &now
	j.Error = nil
}

func (j *CobJobRun) Succeed(records int, summary *string, now time.Time) {
	j.Status = JobCompleted
	j.RecordsProcessed = records
	j.Summary = summary
	j.FinishedAt = &now
}

func (j *CobJobRun) Fail(err string, now time.Time) {
	if len(err) > 1000 {
		err = err[:1000]
	}
	j.Status = JobFailed
	j.Error = &err
	j.FinishedAt = &now
}

// DailyBalance is the end-of-day balance of one ledger account (T24: ACCT.ACTIVITY / EB.CONTRACT.BALANCES).
type DailyBalance struct {
	AccountID    uuid.UUID `db:"account_id" json:"accountId"`
	BusinessDate Date      `db:"business_date" json:"businessDate"`
	Currency     string    `db:"currency" json:"currency"`
	Opening      int64     `db:"opening" json:"opening"`
	Debits       int64     `db:"debits" json:"debits"`
	Credits      int64     `db:"credits" json:"credits"`
	Closing      int64     `db:"closing" json:"closing"`
}

// InterestAccrual is the per-account daily accrual sub-ledger; capitalised at month end.
type InterestAccrual struct {
	ID           uuid.UUID       `db:"id" json:"id"`
	WalletID     uuid.UUID       `db:"wallet_id" json:"walletId"`
	BusinessDate Date            `db:"business_date" json:"businessDate"`
	Currency     string          `db:"currency" json:"currency"`
	Balance      int64           `db:"balance" json:"balance"`
	AnnualRate   decimal.Decimal `db:"annual_rate" json:"annualRate"`
	// Days is the calendar days covered — a Friday accrual covers the weekend (3 days).
	Days                  int        `db:"days" json:"days"`
	Amount                int64      `db:"amount" json:"amount"`
	Capitalised           bool       `db:"capitalised" json:"capitalised"`
	CapitalisationEntryID *uuid.UUID `db:"capitalisation_entry_id" json:"capitalisationEntryId"`
}

// CalculateInterest is actual/365, truncated to the minor unit (the bank never over-accrues).
func CalculateInterest(balance int64, annualRate decimal.Decimal, days int) int64 {
	if balance <= 0 {
		return 0
	}
	// Exact: the numerator is a finite decimal, and QuoRem at precision 0 truncates the quotient.
	numerator := decimal.NewFromInt(balance).Mul(annualRate).Mul(decimal.NewFromInt(int64(days)))
	quotient, _ := numerator.QuoRem(decimal.NewFromInt(365), 0)
	return quotient.IntPart()
}

// ---------------------------------------------------------------------------------------------
// Governance: maker-checker, parameters and overrides

// AuthorisationStatus is the T24 record status: INAU (input, unauthorised), live (authorised) or rejected.
type AuthorisationStatus string

const (
	Unauthorised AuthorisationStatus = "Unauthorised"
	Authorised   AuthorisationStatus = "Authorised"
	Rejected     AuthorisationStatus = "Rejected"
)

// EnsureFourEyes is the rule shared by every maker-checker record.
func EnsureFourEyes(status AuthorisationStatus, inputter, authoriser string) error {
	if status != Unauthorised {
		return Conflict("not_unauthorised", "Record is already %s.", status)
	}
	if strings.EqualFold(inputter, authoriser) {
		return Rule("self_authorisation", "The inputter cannot authorise their own record (four-eyes principle).")
	}
	return nil
}

type AdjustmentLine struct {
	AccountNumber string         `json:"accountNumber"`
	Direction     EntryDirection `json:"direction"`
	Amount        int64          `json:"amount"`
}

// AdjustmentLines is stored as a JSON column.
type AdjustmentLines []AdjustmentLine

func (l AdjustmentLines) Value() (driver.Value, error) {
	b, err := json.Marshal(l)
	return string(b), err
}

func (l *AdjustmentLines) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return json.Unmarshal([]byte(v), l)
	case []byte:
		return json.Unmarshal(v, l)
	}
	return fmt.Errorf("cannot scan %T into AdjustmentLines", src)
}

// ManualAdjustment is a manual journal entry against internal / GL accounts under four-eyes
// control: one operator inputs it (INAU) and a different operator must authorise it before
// anything is posted.
type ManualAdjustment struct {
	ID              uuid.UUID           `db:"id" json:"id"`
	Version         int64               `db:"version" json:"version"`
	Currency        string              `db:"currency" json:"currency"`
	Narrative       string              `db:"narrative" json:"narrative"`
	Lines           AdjustmentLines     `db:"entry_lines" json:"lines"`
	Status          AuthorisationStatus `db:"status" json:"status"`
	Inputter        string              `db:"inputter" json:"inputter"`
	InputAt         time.Time           `db:"input_at" json:"inputAt"`
	Authoriser      *string             `db:"authoriser" json:"authoriser"`
	AuthorisedAt    *time.Time          `db:"authorised_at" json:"authorisedAt"`
	RejectionReason *string             `db:"rejection_reason" json:"rejectionReason"`
	JournalEntryID  *uuid.UUID          `db:"journal_entry_id" json:"journalEntryId"`
}

func NewManualAdjustment(currency, narrative string, lines []AdjustmentLine, inputter string, now time.Time) (*ManualAdjustment, error) {
	if len(lines) < 2 {
		return nil, Rule("invalid_adjustment", "An adjustment needs at least two lines.")
	}
	var debits, credits int64
	for _, l := range lines {
		if l.Amount <= 0 {
			return nil, Rule("invalid_adjustment", "Line amounts must be positive.")
		}
		if l.Direction == Debit {
			debits += l.Amount
		} else {
			credits += l.Amount
		}
	}
	if debits != credits {
		return nil, Rule("unbalanced_entry", "Debits %d must equal credits %d.", debits, credits)
	}
	if strings.TrimSpace(narrative) == "" {
		return nil, Rule("invalid_adjustment", "A narrative is required.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &ManualAdjustment{
		ID: NewID(), Currency: ccy.Code, Narrative: narrative, Lines: lines,
		Status: Unauthorised, Inputter: inputter, InputAt: now,
	}, nil
}

func (m *ManualAdjustment) JournalReference() string { return "adjustment:" + N(m.ID) }

func (m *ManualAdjustment) EnsureUnauthorised(authoriser string) error {
	return EnsureFourEyes(m.Status, m.Inputter, authoriser)
}

func (m *ManualAdjustment) Authorise(authoriser string, journalEntryID uuid.UUID, now time.Time) error {
	if err := m.EnsureUnauthorised(authoriser); err != nil {
		return err
	}
	m.Status = Authorised
	m.Authoriser = &authoriser
	m.AuthorisedAt = &now
	m.JournalEntryID = &journalEntryID
	return nil
}

func (m *ManualAdjustment) Reject(authoriser, reason string, now time.Time) error {
	if err := m.EnsureUnauthorised(authoriser); err != nil {
		return err
	}
	m.Status = Rejected
	m.Authoriser = &authoriser
	m.AuthorisedAt = &now
	m.RejectionReason = &reason
	return nil
}

func (m ManualAdjustment) MarshalJSON() ([]byte, error) {
	type alias ManualAdjustment
	return json.Marshal(struct {
		alias
		JournalReference string `json:"journalReference"`
	}{alias(m), m.JournalReference()})
}

// Well-known charge codes used by the payment modules.
const (
	ChargeCardCollection        = "CARDCOLL"
	ChargeMobileMoneyCollection = "MMCOLL"
	ChargeStablecoinCollection  = "USDCOLL"
	ChargeMobileMoneyPayout     = "MMPAYOUT"
	ChargeBankPayout            = "BANKPAYOUT"
	ChargeFxSpread              = "FXSPREAD"
)

// ChargeType is a parameterised charge (T24: FT.COMMISSION.TYPE). Pricing lives in data, not
// code, and changes go through maker-checker — product teams can reprice without a deployment.
type ChargeType struct {
	ID          uuid.UUID `db:"id" json:"id"`
	Version     int64     `db:"version" json:"version"`
	Code        string    `db:"code" json:"code"`
	Description string    `db:"description" json:"description"`
	BasisPoints int       `db:"basis_points" json:"basisPoints"`
}

func NewChargeType(code, description string, basisPoints int) (*ChargeType, error) {
	c := &ChargeType{ID: NewID(), Code: code}
	return c, c.Update(description, basisPoints)
}

func (c *ChargeType) HistoryName() string { return "ChargeType" }
func (c *ChargeType) Rule() FeeRule       { return FeeRule{BasisPoints: c.BasisPoints} }

func (c *ChargeType) Update(description string, basisPoints int) error {
	if basisPoints < 0 || basisPoints > 2_000 {
		return Rule("invalid_charge", "Charge must be between 0 and 2000 basis points.")
	}
	c.Description = description
	c.BasisPoints = basisPoints
	return nil
}

func (c ChargeType) MarshalJSON() ([]byte, error) {
	type alias ChargeType
	return json.Marshal(struct {
		alias
		Rule FeeRule `json:"rule"`
	}{alias(c), c.Rule()})
}

// InterestCondition is the credit interest paid on an account category in a currency (T24: GROUP.CREDIT.INT).
type InterestCondition struct {
	ID       uuid.UUID `db:"id" json:"id"`
	Version  int64     `db:"version" json:"version"`
	Category int       `db:"category" json:"category"`
	Currency string    `db:"currency" json:"currency"`
	// AnnualRate is a fraction, e.g. 0.04 = 4% p.a.
	AnnualRate decimal.Decimal `db:"annual_rate" json:"annualRate"`
}

func NewInterestCondition(category int, currency string, annualRate decimal.Decimal) (*InterestCondition, error) {
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	c := &InterestCondition{ID: NewID(), Category: category, Currency: ccy.Code}
	return c, c.Update(annualRate)
}

func (c *InterestCondition) HistoryName() string { return "InterestCondition" }
func (c *InterestCondition) Key() string         { return fmt.Sprintf("%d.%s", c.Category, c.Currency) }

func (c *InterestCondition) Update(annualRate decimal.Decimal) error {
	if annualRate.IsNegative() || annualRate.GreaterThan(decimal.RequireFromString("0.5")) {
		return Rule("invalid_rate", "Annual rate must be between 0 and 0.5 (50%%).")
	}
	c.AnnualRate = annualRate
	return nil
}

func (c InterestCondition) MarshalJSON() ([]byte, error) {
	type alias InterestCondition
	return json.Marshal(struct {
		alias
		Key string `json:"key"`
	}{alias(c), c.Key()})
}

type ParameterType string

const (
	ParamChargeType        ParameterType = "ChargeType"
	ParamInterestCondition ParameterType = "InterestCondition"
	ParamSuspenseType      ParameterType = "SuspenseType"
	ParamHookEndpoint      ParameterType = "HookEndpoint"
	ParamCorrespondent     ParameterType = "Correspondent"
	ParamNostroAccount     ParameterType = "NostroAccount"
	ParamProviderChannel   ParameterType = "ProviderChannel"
	ParamOperator          ParameterType = "Operator"
	ParamLocalTable        ParameterType = "LocalTable"
	ParamTransactionType   ParameterType = "TransactionType"
	ParamVersion           ParameterType = "Version"
	ParamEnquiry           ParameterType = "Enquiry"
	// Operations held for authorisation like parameter changes (T24: INAU on the application).
	ParamCustomerTier       ParameterType = "CustomerTier"
	ParamPostingRestriction ParameterType = "PostingRestriction"
	ParamBlocklist          ParameterType = "Blocklist"
	ParamDispute            ParameterType = "Dispute"
	ParamDisputeResolution  ParameterType = "DisputeResolution"
	ParamSettlement         ParameterType = "Settlement"
	ParamKycDecision        ParameterType = "KycDecision"
	ParamCurrencyRates      ParameterType = "CurrencyRates"
	ParamLocalRecord        ParameterType = "LocalRecord"
	ParamOnboardingPolicy   ParameterType = "OnboardingPolicy"
	ParamCustomerDetails    ParameterType = "CustomerDetails"
)

// ParameterChangeRequest is a proposed parameter change awaiting authorisation (T24: the INAU
// copy of a parameter record). Nothing changes until a second operator authorises it.
type ParameterChangeRequest struct {
	ID              uuid.UUID           `db:"id" json:"id"`
	Version         int64               `db:"version" json:"version"`
	Type            ParameterType       `db:"type" json:"type"`
	Key             string              `db:"param_key" json:"key"`
	Payload         string              `db:"payload" json:"payload"`
	Status          AuthorisationStatus `db:"status" json:"status"`
	Inputter        string              `db:"inputter" json:"inputter"`
	InputAt         time.Time           `db:"input_at" json:"inputAt"`
	Authoriser      *string             `db:"authoriser" json:"authoriser"`
	AuthorisedAt    *time.Time          `db:"authorised_at" json:"authorisedAt"`
	RejectionReason *string             `db:"rejection_reason" json:"rejectionReason"`
}

func (r *ParameterChangeRequest) Authorise(authoriser string, now time.Time) error {
	if err := EnsureFourEyes(r.Status, r.Inputter, authoriser); err != nil {
		return err
	}
	r.Status = Authorised
	r.Authoriser = &authoriser
	r.AuthorisedAt = &now
	return nil
}

func (r *ParameterChangeRequest) Reject(authoriser, reason string, now time.Time) error {
	if err := EnsureFourEyes(r.Status, r.Inputter, authoriser); err != nil {
		return err
	}
	r.Status = Rejected
	r.Authoriser = &authoriser
	r.AuthorisedAt = &now
	r.RejectionReason = &reason
	return nil
}

// Override is a T24-style warning the caller must explicitly accept for a transaction to
// proceed (e.g. a large amount). Accepted overrides are recorded with the transaction.
type Override struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
