package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type CustomerType string

const (
	Individual CustomerType = "Individual"
	Business   CustomerType = "Business"
	// FinancialInstitution is a correspondent bank or PSP that holds a vostro account with us.
	FinancialInstitution CustomerType = "FinancialInstitution"
)

// IsCorporate: businesses and financial institutions are verified by KYB (registration, directors).
func (t CustomerType) IsCorporate() bool { return t == Business || t == FinancialInstitution }

type KycStatus string

const (
	KycNotStarted KycStatus = "NotStarted"
	KycPending    KycStatus = "Pending"
	KycVerified   KycStatus = "Verified"
	KycRejected   KycStatus = "Rejected"
)

// Customer is an individual (KYC) or business (KYB) customer — the T24 CUSTOMER record.
// Verification status drives the customer's tier, and the tier drives transaction limits.
type Customer struct {
	ID      uuid.UUID `db:"id" json:"id"`
	Version int64     `db:"version" json:"version"`
	// CustomerNumber is the T24 customer id: numeric and sequential, e.g. 100001.
	CustomerNumber int64 `db:"customer_number" json:"customerNumber"`
	// Mnemonic is a short, unique, human-friendly search key, e.g. AMANI001.
	Mnemonic string `db:"mnemonic" json:"mnemonic"`
	// Sector: 1001 = individual, 2001 = corporate, 3001 = bank / financial institution.
	Sector int `db:"sector" json:"sector"`
	// AccountOfficer is the relationship manager / DAO responsible.
	AccountOfficer       int          `db:"account_officer" json:"accountOfficer"`
	Type                 CustomerType `db:"type" json:"type"`
	LegalName            string       `db:"legal_name" json:"legalName"`
	Email                string       `db:"email" json:"email"`
	Phone                *string      `db:"phone" json:"phone"`
	Country              string       `db:"country" json:"country"`
	KycStatus            KycStatus    `db:"kyc_status" json:"kycStatus"`
	Tier                 int          `db:"tier" json:"tier"`
	KycProviderReference *string      `db:"kyc_provider_reference" json:"kycProviderReference"`
	KycRejectionReason   *string      `db:"kyc_rejection_reason" json:"kycRejectionReason"`
	CreatedAt            time.Time    `db:"created_at" json:"createdAt"`
	// ContactChangedAt is when the phone or email last changed: payouts soon after are reviewed.
	ContactChangedAt *time.Time `db:"contact_changed_at" json:"contactChangedAt"`
}

func NewCustomer(number int64, typ CustomerType, legalName, email string, phone *string, country string, accountOfficer int, now time.Time) (*Customer, error) {
	legalName = strings.TrimSpace(legalName)
	if legalName == "" {
		return nil, Rule("invalid_customer", "Legal name is required.")
	}
	if len(country) != 2 {
		return nil, Rule("invalid_customer", "Country must be an ISO-3166 alpha-2 code.")
	}
	sector := map[CustomerType]int{Individual: 1001, Business: 2001, FinancialInstitution: 3001}[typ]
	if sector == 0 {
		return nil, Rule("invalid_customer", "Unknown customer type '%s'.", typ)
	}
	return &Customer{
		ID: NewID(), CustomerNumber: number, Type: typ, LegalName: legalName, Mnemonic: buildMnemonic(legalName, number),
		Sector: sector, AccountOfficer: accountOfficer, Email: strings.ToLower(strings.TrimSpace(email)), Phone: phone,
		Country: strings.ToUpper(country), KycStatus: KycNotStarted, CreatedAt: now,
	}, nil
}

func (c *Customer) HistoryName() string { return "Customer" }

// CustomerDetails is a change to a customer's details; a nil field is left as it is.
type CustomerDetails struct {
	LegalName *string `json:"legalName,omitempty"`
	Email     *string `json:"email,omitempty"`
	Phone     *string `json:"phone,omitempty"`
	Country   *string `json:"country,omitempty"`
}

// Normalise trims the change and drops fields that already hold the customer's value.
func (d CustomerDetails) Normalise(c *Customer) (CustomerDetails, error) {
	var out CustomerDetails
	if d.LegalName != nil {
		name := strings.TrimSpace(*d.LegalName)
		if name == "" {
			return out, Rule("invalid_customer", "Legal name is required.")
		}
		if name != c.LegalName {
			out.LegalName = &name
		}
	}
	if d.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*d.Email))
		if email == "" {
			return out, Rule("invalid_customer", "Email is required.")
		}
		if email != c.Email {
			out.Email = &email
		}
	}
	if d.Phone != nil {
		phone := strings.TrimSpace(*d.Phone)
		if c.Phone == nil || phone != *c.Phone {
			out.Phone = &phone
		}
	}
	if d.Country != nil {
		country := strings.ToUpper(strings.TrimSpace(*d.Country))
		if len(country) != 2 {
			return out, Rule("invalid_customer", "Country must be an ISO-3166 alpha-2 code.")
		}
		if country != c.Country {
			out.Country = &country
		}
	}
	if out == (CustomerDetails{}) {
		return out, Rule("no_change", "The details given are the customer's current details.")
	}
	return out, nil
}

// Identity is the part of the change KYC verified: the legal name and country.
func (d CustomerDetails) Identity() CustomerDetails {
	return CustomerDetails{LegalName: d.LegalName, Country: d.Country}
}

// Contact is the part of the change that reaches the customer: the email and phone.
func (d CustomerDetails) Contact() CustomerDetails {
	return CustomerDetails{Email: d.Email, Phone: d.Phone}
}

// Apply writes the change onto the customer, stamping a contact change.
func (c *Customer) Apply(d CustomerDetails, now time.Time) {
	if d.LegalName != nil {
		c.LegalName = *d.LegalName
	}
	if d.Country != nil {
		c.Country = *d.Country
	}
	if d.Email != nil {
		c.Email = *d.Email
	}
	if d.Phone != nil {
		phone := *d.Phone
		if phone == "" {
			c.Phone = nil
		} else {
			c.Phone = &phone
		}
	}
	if d.Email != nil || d.Phone != nil {
		c.ContactChangedAt = &now
	}
}

func (c *Customer) SubmitKyc() error {
	if c.KycStatus == KycVerified {
		return Conflict("kyc_already_verified", "Customer is already verified.")
	}
	c.KycStatus = KycPending
	c.KycRejectionReason = nil
	return nil
}

func (c *Customer) ApproveKyc(providerReference string) {
	c.KycStatus = KycVerified
	c.KycProviderReference = &providerReference
	// Verified businesses start on a higher tier: KYB includes directors and UBOs.
	floor := 1
	if c.Type.IsCorporate() {
		floor = 2
	}
	c.Tier = max(c.Tier, floor)
}

func (c *Customer) RejectKyc(reason string) {
	c.KycStatus = KycRejected
	c.KycRejectionReason = &reason
	c.Tier = 0
}

func (c *Customer) SetTier(tier int) error {
	if tier < 0 || tier > MaxTier {
		return Rule("invalid_tier", "Tier must be between 0 and %d.", MaxTier)
	}
	if tier > 0 && c.KycStatus != KycVerified {
		return Rule("kyc_not_verified", "Only verified customers can be assigned a tier above 0.")
	}
	c.Tier = tier
	return nil
}

func buildMnemonic(name string, number int64) string {
	var letters strings.Builder
	for _, r := range name {
		if letters.Len() == 6 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters.WriteRune(r)
		}
	}
	prefix := strings.ToUpper(letters.String())
	if prefix == "" {
		prefix = "CUST"
	}
	return fmt.Sprintf("%s%03d", prefix, number%1000)
}

// TierLimits are outgoing-money limits per tier, expressed in USD and converted at mid-rate.
type TierLimits struct {
	SingleTransactionUSD decimal.Decimal
	RollingDailyUSD      decimal.Decimal
}

const MaxTier = 3

func LimitsForTier(tier int) (TierLimits, error) {
	usd := func(single, daily int64) TierLimits {
		return TierLimits{decimal.NewFromInt(single), decimal.NewFromInt(daily)}
	}
	switch tier {
	case 0:
		return usd(0, 0), nil // unverified: can receive, cannot send
	case 1:
		return usd(1_000, 3_000), nil // basic KYC
	case 2:
		return usd(10_000, 50_000), nil // full KYC / KYB
	case 3:
		return usd(100_000, 500_000), nil // enhanced due diligence
	}
	return TierLimits{}, Rule("invalid_tier", "Unknown tier %d.", tier)
}

// ---------------------------------------------------------------------------------------------
// Accounts (wallets)

// PostingRestriction is T24's POSTING.RESTRICT.
type PostingRestriction string

const (
	RestrictNone PostingRestriction = "None"
	DebitBlock   PostingRestriction = "DebitBlock"
	CreditBlock  PostingRestriction = "CreditBlock"
	FullBlock    PostingRestriction = "FullBlock"
)

// Wallet is a customer account (T24: ACCOUNT) — a stored-value wallet for individuals or a
// merchant current account for businesses. It owns two ledger accounts: available (T24 working
// balance) and held (T24 locked amounts — card authorisations and disputes). Online actual
// balance = available + held. The wallet never stores a balance itself; the ledger is the
// source of truth.
type Wallet struct {
	ID                 uuid.UUID          `db:"id" json:"id"`
	Version            int64              `db:"version" json:"version"`
	CustomerID         uuid.UUID          `db:"customer_id" json:"customerId"`
	AccountNumber      string             `db:"account_number" json:"accountNumber"`
	Title              string             `db:"title" json:"title"`
	Category           int                `db:"category" json:"category"`
	Currency           string             `db:"currency" json:"currency"`
	AvailableAccountID uuid.UUID          `db:"available_account_id" json:"availableAccountId"`
	HeldAccountID      uuid.UUID          `db:"held_account_id" json:"heldAccountId"`
	Restriction        PostingRestriction `db:"restriction" json:"restriction"`
	RestrictionReason  *string            `db:"restriction_reason" json:"restrictionReason"`
	OpeningDate        Date               `db:"opening_date" json:"openingDate"`
	CreatedAt          time.Time          `db:"created_at" json:"createdAt"`
	// ClosedAt is set when the customer closed the account; it is then blocked for good.
	ClosedAt      *time.Time `db:"closed_at" json:"closedAt"`
	ClosureReason *string    `db:"closure_reason" json:"closureReason"`
	// A sub-account belongs to the same customer as its main account (ParentWalletID), in the
	// same currency, and is used by someone else: HolderName, e.g. the customer's child.
	ParentWalletID     *uuid.UUID `db:"parent_wallet_id" json:"parentAccountId"`
	HolderName         *string    `db:"holder_name" json:"holderName"`
	HolderRelationship *string    `db:"holder_relationship" json:"holderRelationship"`
}

// Relationships of a sub-account's holder to the customer.
var HolderRelationships = []string{"spouse", "child", "parent", "sibling", "relative", "friend", "employee", "other"}

// MaxSubAccounts is how many sub-accounts one main account may have.
const MaxSubAccounts = 10

// OpenSubAccount opens an account under a main account, for someone the customer names. It has
// its own number and balance; the customer's KYC tier limits cover it with their other accounts.
func OpenSubAccount(parent *Wallet, c *Customer, holderName, relationship, accountNumber string, businessDate Date, now time.Time) (*Wallet, *LedgerAccount, *LedgerAccount, error) {
	if err := parent.EnsureOpen(); err != nil {
		return nil, nil, nil, err
	}
	if parent.ParentWalletID != nil {
		return nil, nil, nil, Rule("sub_account_of_sub_account", "Account %s is itself a sub-account; open sub-accounts under the main account.", parent.AccountNumber)
	}
	holderName = strings.TrimSpace(holderName)
	if len(holderName) < 2 || len(holderName) > 100 {
		return nil, nil, nil, Rule("invalid_holder", "The holder's name must be 2 to 100 characters.")
	}
	relationship = strings.ToLower(strings.TrimSpace(relationship))
	if !slices.Contains(HolderRelationships, relationship) {
		return nil, nil, nil, Rule("invalid_relationship", "Relationship must be one of %s.", strings.Join(HolderRelationships, ", "))
	}
	w, available, held, err := OpenWallet(c, parent.Currency, accountNumber, businessDate, now)
	if err != nil {
		return nil, nil, nil, err
	}
	w.Title = fmt.Sprintf("%s — %s", c.LegalName, holderName)
	w.Category = parent.Category
	available.Category, held.Category = parent.Category, parent.Category
	w.ParentWalletID, w.HolderName, w.HolderRelationship = &parent.ID, &holderName, &relationship
	return w, available, held, nil
}

func (w *Wallet) HistoryName() string { return "Wallet" }

// Close closes the account for good: every posting is refused from now on.
func (w *Wallet) Close(reason string, now time.Time) error {
	if w.ClosedAt != nil {
		return Conflict("account_closed", "Account %s is already closed.", w.AccountNumber)
	}
	w.ClosedAt, w.ClosureReason = &now, &reason
	w.Restrict(FullBlock, "Closed: "+reason)
	return nil
}

// EnsureOpen refuses changes to a closed account.
func (w *Wallet) EnsureOpen() error {
	if w.ClosedAt != nil {
		return Conflict("account_closed", "Account %s was closed on %s.", w.AccountNumber, w.ClosedAt.Format("2006-01-02"))
	}
	return nil
}

// OpenWallet opens an account for a customer with its available and held ledger accounts.
func OpenWallet(c *Customer, currency, accountNumber string, businessDate Date, now time.Time) (*Wallet, *LedgerAccount, *LedgerAccount, error) {
	category := CategoryMobileWallet
	switch c.Type {
	case Business:
		category = CategoryMerchantCurrentAccount
	case FinancialInstitution:
		category = CategoryVostro // a correspondent's account with us: a vostro (due to banks)
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, nil, nil, err
	}
	w := &Wallet{
		ID: NewID(), CustomerID: c.ID, AccountNumber: accountNumber, Title: c.LegalName, Category: category,
		Currency: ccy.Code, Restriction: RestrictNone, OpeningDate: businessDate, CreatedAt: now,
	}
	available, err := NewLedgerAccount(WalletAvailableCode(w.ID), accountNumber, fmt.Sprintf("%s (%s)", c.LegalName, ccy.Code), Liability, category, ccy.Code, false, now)
	if err != nil {
		return nil, nil, nil, err
	}
	held, err := NewLedgerAccount(WalletHeldCode(w.ID), accountNumber+"-LCK", fmt.Sprintf("%s (%s) locked", c.LegalName, ccy.Code), Liability, category, ccy.Code, false, now)
	if err != nil {
		return nil, nil, nil, err
	}
	w.AvailableAccountID, w.HeldAccountID = available.ID, held.ID
	return w, available, held, nil
}

func (w *Wallet) CanDebit() bool {
	return w.Restriction != DebitBlock && w.Restriction != FullBlock
}

func (w *Wallet) EnsureCanDebit() error {
	if err := w.EnsureOpen(); err != nil {
		return err
	}
	if !w.CanDebit() {
		return Rule("account_restricted", "Account %s does not allow debits (%s: %s).", w.AccountNumber, w.Restriction, deref(w.RestrictionReason))
	}
	return nil
}

func (w *Wallet) EnsureCanCredit() error {
	if err := w.EnsureOpen(); err != nil {
		return err
	}
	if w.Restriction == CreditBlock || w.Restriction == FullBlock {
		return Rule("account_restricted", "Account %s does not allow credits (%s: %s).", w.AccountNumber, w.Restriction, deref(w.RestrictionReason))
	}
	return nil
}

func (w *Wallet) Restrict(r PostingRestriction, reason string) {
	w.Restriction = r
	if r == RestrictNone {
		w.RestrictionReason = nil
	} else {
		w.RestrictionReason = &reason
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------------------------------------
// Book transfers

type TransferStatus string

const (
	TransferInitiated TransferStatus = "Initiated"
	TransferCompleted TransferStatus = "Completed"
	TransferDeclined  TransferStatus = "Declined"
)

// Transfer is an instant wallet-to-wallet (book) transfer within the platform. Transfers are
// synchronous and atomic: they either post to the ledger or are declined. Declines are
// persisted too — they are signal for fraud and support teams.
type Transfer struct {
	ID                  uuid.UUID      `db:"id" json:"id"`
	SourceWalletID      uuid.UUID      `db:"source_wallet_id" json:"sourceWalletId"`
	DestinationWalletID uuid.UUID      `db:"destination_wallet_id" json:"destinationWalletId"`
	Amount              int64          `db:"amount" json:"amount"`
	Currency            string         `db:"currency" json:"currency"`
	Narration           *string        `db:"narration" json:"narration"`
	Status              TransferStatus `db:"status" json:"status"`
	DeclineCode         *string        `db:"decline_code" json:"declineCode"`
	JournalEntryID      *uuid.UUID     `db:"journal_entry_id" json:"journalEntryId"`
	CreatedAt           time.Time      `db:"created_at" json:"createdAt"`
	TransactionID       *uuid.UUID     `db:"transaction_id" json:"transactionId,omitempty"`
}

func NewTransfer(source, destination uuid.UUID, amount int64, currency string, narration *string, now time.Time) (*Transfer, error) {
	if amount <= 0 {
		return nil, Rule("invalid_amount", "Amount must be greater than zero.")
	}
	if source == destination {
		return nil, Rule("invalid_transfer", "Source and destination wallets must differ.")
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &Transfer{ID: NewID(), SourceWalletID: source, DestinationWalletID: destination, Amount: amount,
		Currency: ccy.Code, Narration: narration, Status: TransferInitiated, CreatedAt: now}, nil
}

func (t *Transfer) JournalReference() string { return "transfer:" + N(t.ID) }

func (t *Transfer) Complete(entryID uuid.UUID) {
	t.Status = TransferCompleted
	t.JournalEntryID = &entryID
}

func (t *Transfer) Decline(code string) {
	t.Status = TransferDeclined
	t.DeclineCode = &code
}

func (t Transfer) MarshalJSON() ([]byte, error) {
	type alias Transfer
	return json.Marshal(struct {
		alias
		JournalReference string `json:"journalReference"`
	}{alias(t), t.JournalReference()})
}

// ---------------------------------------------------------------------------------------------
// FX

type FxQuoteStatus string

const (
	QuoteOpen     FxQuoteStatus = "Open"
	QuoteExecuted FxQuoteStatus = "Executed"
)

// FxQuote is a short-lived, firm FX quote. The customer sees exactly what they will receive
// before committing; the platform's margin is an explicit fee in the source currency rather
// than a hidden rate markup, which keeps FX revenue visible in the ledger.
type FxQuote struct {
	ID             uuid.UUID       `db:"id" json:"id"`
	Version        int64           `db:"version" json:"version"`
	CustomerID     uuid.UUID       `db:"customer_id" json:"customerId"`
	SourceCurrency string          `db:"source_currency" json:"sourceCurrency"`
	TargetCurrency string          `db:"target_currency" json:"targetCurrency"`
	SourceAmount   int64           `db:"source_amount" json:"sourceAmount"`
	Fee            int64           `db:"fee" json:"fee"`
	MidRate        decimal.Decimal `db:"mid_rate" json:"midRate"`
	SpreadBps      int             `db:"spread_bps" json:"spreadBps"`
	TargetAmount   int64           `db:"target_amount" json:"targetAmount"`
	Status         FxQuoteStatus   `db:"status" json:"status"`
	CreatedAt      time.Time       `db:"created_at" json:"createdAt"`
	ExpiresAt      time.Time       `db:"expires_at" json:"expiresAt"`
	ExecutedAt     *time.Time      `db:"executed_at" json:"executedAt"`
	SourceWalletID *uuid.UUID      `db:"source_wallet_id" json:"sourceWalletId"`
	TargetWalletID *uuid.UUID      `db:"target_wallet_id" json:"targetWalletId"`
}

func NewFxQuote(customerID uuid.UUID, sourceCurrency, targetCurrency string, sourceAmount int64, midRate decimal.Decimal, spreadBps int, ttl time.Duration, now time.Time) (*FxQuote, error) {
	source, err := CurrencyOf(sourceCurrency)
	if err != nil {
		return nil, err
	}
	target, err := CurrencyOf(targetCurrency)
	if err != nil {
		return nil, err
	}
	if source == target {
		return nil, Rule("invalid_fx_pair", "Source and target currency must differ.")
	}
	if sourceAmount <= 0 {
		return nil, Rule("invalid_amount", "Amount must be greater than zero.")
	}
	if !midRate.IsPositive() {
		return nil, Rule("invalid_rate", "Rate must be positive.")
	}
	fee := FeeRule{BasisPoints: spreadBps}.Calculate(sourceAmount)
	// Truncate the converted amount: the platform never gives away a fraction of a minor unit.
	targetAmount := target.ToMinorTruncated(source.ToMajor(sourceAmount - fee).Mul(midRate))
	if targetAmount <= 0 {
		return nil, Rule("amount_too_small", "Converted amount rounds to zero.")
	}
	return &FxQuote{
		ID: NewID(), CustomerID: customerID, SourceCurrency: source.Code, TargetCurrency: target.Code, SourceAmount: sourceAmount,
		Fee: fee, MidRate: midRate, SpreadBps: spreadBps, TargetAmount: targetAmount, Status: QuoteOpen,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}, nil
}

func (q *FxQuote) NetSourceAmount() int64 { return q.SourceAmount - q.Fee }

func (q *FxQuote) Execute(customerID, sourceWalletID, targetWalletID uuid.UUID, now time.Time) error {
	if customerID != q.CustomerID {
		return Rule("quote_customer_mismatch", "Quote belongs to a different customer.")
	}
	if q.Status == QuoteExecuted {
		return Conflict("quote_already_executed", "This quote has already been used.")
	}
	if !now.Before(q.ExpiresAt) {
		return Rule("quote_expired", "This quote has expired; request a new one.")
	}
	q.Status = QuoteExecuted
	q.ExecutedAt = &now
	q.SourceWalletID = &sourceWalletID
	q.TargetWalletID = &targetWalletID
	return nil
}

func (q FxQuote) MarshalJSON() ([]byte, error) {
	type alias FxQuote
	return json.Marshal(struct {
		alias
		NetSourceAmount int64 `json:"netSourceAmount"`
	}{alias(q), q.NetSourceAmount()})
}

// OnboardingPolicy is how customers sign up: whether KYC details are mandatory. There is one
// record (code SYSTEM), amended by administrators under maker-checker; until one is authorised
// the default applies.
type OnboardingPolicy struct {
	ID          uuid.UUID `db:"id" json:"id"`
	Version     int64     `db:"version" json:"version"`
	Code        string    `db:"code" json:"-"`
	KycRequired bool      `db:"kyc_required" json:"kycRequired"`
	Reason      string    `db:"reason" json:"reason"`
	UpdatedBy   string    `db:"updated_by" json:"updatedBy"`
	UpdatedAt   time.Time `db:"updated_at" json:"updatedAt"`
}

// DefaultOnboardingPolicy applies until administrators authorise one: KYC is required.
func DefaultOnboardingPolicy() OnboardingPolicy {
	return OnboardingPolicy{Code: "SYSTEM", KycRequired: true, Reason: "default", UpdatedBy: "system"}
}

func (p *OnboardingPolicy) HistoryName() string { return "OnboardingPolicy" }

// EnsureKyc refuses an account for a customer who has not submitted KYC, or was refused, when
// the policy requires it. A submission still under review is enough.
func (p OnboardingPolicy) EnsureKyc(c *Customer) error {
	if p.KycRequired && (c.KycStatus == KycNotStarted || c.KycStatus == KycRejected) {
		return Rule("kyc_required", "KYC is required before an account is opened; submit the customer's KYC first.")
	}
	return nil
}
