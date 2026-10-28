package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AccountType string

const (
	Asset     AccountType = "Asset"
	Liability AccountType = "Liability"
	Equity    AccountType = "Equity"
	Revenue   AccountType = "Revenue"
	Expense   AccountType = "Expense"
)

// IsDebitNormal reports whether debits increase the balance (assets and expenses).
func (t AccountType) IsDebitNormal() bool { return t == Asset || t == Expense }

type EntryDirection string

const (
	Debit  EntryDirection = "Debit"
	Credit EntryDirection = "Credit"
)

// LedgerAccount is a single-currency account in the general ledger. Balance is a denormalised
// running total in the account's normal direction (debit-normal for assets/expenses,
// credit-normal for everything else), protected by optimistic concurrency. It can always be
// re-derived from journal lines, which is exactly what the COB ledger-integrity job does.
type LedgerAccount struct {
	ID      uuid.UUID `db:"id" json:"id"`
	Version int64     `db:"version" json:"version"`
	// Code is the semantic key used by services, e.g. revenue:fees:KES.
	Code string `db:"code" json:"code"`
	// AccountNumber is T24-style: customer 1000000018, internal KES140010001, P&L PL52000KES.
	AccountNumber string      `db:"account_number" json:"accountNumber"`
	Name          string      `db:"name" json:"name"`
	Type          AccountType `db:"type" json:"type"`
	// Category is the product / internal / P&L category (T24: CATEGORY).
	Category int    `db:"category" json:"category"`
	GLCode   string `db:"gl_code" json:"glCode"`
	Currency string `db:"currency" json:"currency"`
	Balance  int64  `db:"balance" json:"balance"`
	// AllowNegative: customer funds must never go negative; system/clearing accounts may.
	AllowNegative bool `db:"allow_negative" json:"allowNegative"`
	// ParentID is set on a part of a split account: a full account in the same
	// category, GL line and currency. The account's balance is the sum over it and its parts.
	ParentID  *uuid.UUID `db:"parent_id" json:"parentId,omitempty"`
	CreatedAt time.Time  `db:"created_at" json:"createdAt"`
}

// Splits reports an account that most units of work post to, split into parts on the server
// engines so they don't queue on one row: provider floats and books, fee income and payouts in
// transit.
func (a *LedgerAccount) Splits() bool {
	if a.ParentID != nil {
		return false
	}
	switch a.Category {
	case CategoryProviderFloat, CategoryFeeIncome, CategoryPayoutsInTransit:
		return true
	}
	return a.GLCode == GLChannelInward || a.GLCode == GLChannelOutward
}

// Split reports an account that is, or belongs to, a split account.
func (a *LedgerAccount) Split() bool { return a.ParentID != nil || a.Splits() }

// Group is the account a part belongs to; an account's own id otherwise.
func (a *LedgerAccount) Group() uuid.UUID {
	if a.ParentID != nil {
		return *a.ParentID
	}
	return a.ID
}

// PartCode is the code of part n of a split account.
func PartCode(code string, n int) string { return code + "#" + strconv.Itoa(n) }

// NewPart opens part n of a split account.
func (a *LedgerAccount) NewPart(n int, now time.Time) *LedgerAccount {
	return &LedgerAccount{ID: NewID(), Code: PartCode(a.Code, n), AccountNumber: a.AccountNumber + "-" + strconv.Itoa(n),
		Name: fmt.Sprintf("%s (part %d)", a.Name, n), Type: a.Type, Category: a.Category, GLCode: a.GLCode, Currency: a.Currency,
		AllowNegative: a.AllowNegative, ParentID: &a.ID, CreatedAt: now}
}

// IsWalletAccount reports a customer account's available or held balance: customer funds, which
// a posting checks under the account's version, as every other account would contend for it.
func (a *LedgerAccount) IsWalletAccount() bool { return strings.HasPrefix(a.Code, "liability:wallet:") }

func NewLedgerAccount(code, number, name string, typ AccountType, category int, currency string, allowNegative bool, now time.Time) (*LedgerAccount, error) {
	gl, err := GLFor(category)
	if err != nil {
		return nil, err
	}
	return NewLedgerAccountInGL(code, number, name, typ, category, gl, currency, allowNegative, now)
}

// NewLedgerAccountInGL opens an account in a category whose GL line the caller has already
// resolved — needed for categories allocated at runtime (see Chart).
func NewLedgerAccountInGL(code, number, name string, typ AccountType, category int, gl, currency string, allowNegative bool, now time.Time) (*LedgerAccount, error) {
	if _, ok := GLAccountByCode(gl); !ok {
		return nil, Rule("invalid_gl_line", "GL line %s is not in the chart of accounts.", gl)
	}
	ccy, err := CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	return &LedgerAccount{
		ID: NewID(), Code: code, AccountNumber: number, Name: name, Type: typ, Category: category,
		GLCode: gl, Currency: ccy.Code, AllowNegative: allowNegative, CreatedAt: now,
	}, nil
}

func (a *LedgerAccount) IsDebitNormal() bool { return a.Type.IsDebitNormal() }

// SignedDelta is the effect of a posting on this account's normal-direction balance.
func (a *LedgerAccount) SignedDelta(direction EntryDirection, amount int64) int64 {
	if (direction == Debit) == a.IsDebitNormal() {
		return amount
	}
	return -amount
}

func (a LedgerAccount) MarshalJSON() ([]byte, error) {
	type alias LedgerAccount
	return json.Marshal(struct {
		alias
		IsDebitNormal bool `json:"isDebitNormal"`
	}{alias(a), a.IsDebitNormal()})
}

// EntryCategory is the business reason for a journal entry; it drives statements, limits and reporting.
type EntryCategory string

const (
	CatCollection             EntryCategory = "Collection"
	CatRefund                 EntryCategory = "Refund"
	CatRefundReversal         EntryCategory = "RefundReversal"
	CatTransfer               EntryCategory = "Transfer"
	CatPayoutReserve          EntryCategory = "PayoutReserve"
	CatPayoutCompletion       EntryCategory = "PayoutCompletion"
	CatPayoutReversal         EntryCategory = "PayoutReversal"
	CatFxConversion           EntryCategory = "FxConversion"
	CatCardAuthorization      EntryCategory = "CardAuthorization"
	CatCardCapture            EntryCategory = "CardCapture"
	CatCardRelease            EntryCategory = "CardRelease"
	CatDisputeHold            EntryCategory = "DisputeHold"
	CatDisputeRelease         EntryCategory = "DisputeRelease"
	CatDisputeLoss            EntryCategory = "DisputeLoss"
	CatSettlement             EntryCategory = "Settlement"
	CatInterestAccrual        EntryCategory = "InterestAccrual"
	CatInterestCapitalisation EntryCategory = "InterestCapitalisation"
	CatManualAdjustment       EntryCategory = "ManualAdjustment"
	CatDeposit                EntryCategory = "Deposit"
	CatDepositAllocation      EntryCategory = "DepositAllocation"
	CatDepositReversal        EntryCategory = "DepositReversal"
	CatSuspense               EntryCategory = "Suspense"
	CatSuspenseClearing       EntryCategory = "SuspenseClearing"
	CatSuspenseWriteOff       EntryCategory = "SuspenseWriteOff"
	CatChannelTransfer        EntryCategory = "ChannelTransfer"
	// CatOwnTransfer moves money between one customer's own accounts (a parent funding a
	// family member's sub-account): it never leaves the customer, so it is not outgoing.
	CatOwnTransfer EntryCategory = "OwnAccountTransfer"
)

// EntryCategories lists every category (for query-string parsing).
var EntryCategories = []EntryCategory{
	CatCollection, CatRefund, CatRefundReversal, CatTransfer, CatPayoutReserve, CatPayoutCompletion,
	CatPayoutReversal, CatFxConversion, CatCardAuthorization, CatCardCapture, CatCardRelease, CatDisputeHold,
	CatDisputeRelease, CatDisputeLoss, CatSettlement, CatInterestAccrual, CatInterestCapitalisation, CatManualAdjustment, CatDeposit, CatDepositAllocation, CatDepositReversal,
	CatSuspense, CatSuspenseClearing, CatSuspenseWriteOff, CatChannelTransfer, CatOwnTransfer,
}

// OutgoingCategories are journal categories that represent money leaving a customer's control.
var OutgoingCategories = []EntryCategory{CatTransfer, CatPayoutReserve, CatFxConversion, CatCardAuthorization, CatChannelTransfer}

// JournalEntry is an immutable, balanced set of journal lines. Entries are never updated or
// deleted — mistakes are corrected with a compensating entry. Reference is unique, which
// makes every posting idempotent at the database level.
type JournalEntry struct {
	ID uuid.UUID `db:"id" json:"id"`
	// Reference is the internal idempotency key, e.g. payment:{id}.
	Reference string `db:"reference" json:"reference"`
	// TransactionReference is the customer-facing, T24-style reference, e.g. FT26286K3QZ8.
	TransactionReference string        `db:"transaction_reference" json:"transactionReference"`
	Category             EntryCategory `db:"category" json:"category"`
	Description          string        `db:"description" json:"description"`
	// PostedAt is the system timestamp when the entry was written.
	PostedAt time.Time `db:"posted_at" json:"postedAt"`
	// BookingDate is the business date the entry belongs to. During COB this is the next working day.
	BookingDate Date `db:"booking_date" json:"bookingDate"`
	// ValueDate is the date from which the movement counts for interest.
	ValueDate Date `db:"value_date" json:"valueDate"`
	// EntryHash is SHA-256 over the entry and its lines, also sealed into the audit chain, so an
	// edited amount anywhere is detectable.
	EntryHash string `db:"entry_hash" json:"entryHash"`
	// HashVersion 2 (every entry now) seals everything but the lines' running balances, which a
	// busy system account only learns at commit; 1 covered them too.
	HashVersion int `db:"hash_version" json:"hashVersion"`
	// TransactionID is the transaction record the entry belongs to (T24: the STMT.ENTRY's
	// TRANS.REFERENCE), when a transaction moved the money.
	TransactionID *uuid.UUID    `db:"transaction_id" json:"transactionId,omitempty"`
	Lines         []JournalLine `db:"-" json:"lines"`
}

type JournalLine struct {
	ID             uuid.UUID      `db:"id" json:"id"`
	JournalEntryID uuid.UUID      `db:"journal_entry_id" json:"journalEntryId"`
	AccountID      uuid.UUID      `db:"account_id" json:"accountId"`
	Direction      EntryDirection `db:"direction" json:"direction"`
	// Amount is always positive; the sign lives in Direction.
	Amount   int64  `db:"amount" json:"amount"`
	Currency string `db:"currency" json:"currency"`
	// BalanceAfter is the account balance immediately after this line — powers running-balance statements.
	BalanceAfter int64 `db:"balance_after" json:"balanceAfter"`
	// BookingDate is denormalised from the entry so COB balance snapshots are a single indexed scan.
	BookingDate Date `db:"booking_date" json:"bookingDate"`
}

const referenceAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// TransactionReferencePrefix is the two-letter T24 application prefix for a category.
func TransactionReferencePrefix(c EntryCategory) string {
	switch c {
	case CatCollection:
		return "PC"
	case CatRefund, CatRefundReversal:
		return "RF"
	case CatTransfer, CatOwnTransfer:
		return "AC"
	case CatPayoutReserve, CatPayoutCompletion, CatPayoutReversal:
		return "PO"
	case CatFxConversion:
		return "FX"
	case CatCardAuthorization, CatCardCapture, CatCardRelease:
		return "CD"
	case CatDisputeHold, CatDisputeRelease, CatDisputeLoss:
		return "DS"
	case CatSettlement:
		return "ST"
	case CatInterestAccrual, CatInterestCapitalisation:
		return "IN"
	case CatDeposit, CatDepositAllocation, CatDepositReversal:
		return "DP"
	case CatManualAdjustment:
		return "AJ"
	case CatSuspense, CatSuspenseClearing, CatSuspenseWriteOff:
		return "SU"
	case CatChannelTransfer:
		return "CH"
	}
	return "TX"
}

// NewTransactionReference is prefix + booking date as YYDDD (Julian day) + five random characters,
// e.g. FT26286K3QZ8. Random suffixes are fine for one-off entries; the posting engine uses
// SequencedTransactionReference, which cannot collide.
func NewTransactionReference(c EntryCategory, bookingDate Date) string {
	return fmt.Sprintf("%s%02d%03d%s", TransactionReferencePrefix(c), bookingDate.Y%100, bookingDate.YearDay(), RandomString(referenceAlphabet, 5))
}

// referenceSpace is the number of distinct five-character suffixes: 32^5 = 2^25.
const referenceSpace = 1 << 25

// SequencedTransactionReference builds a T24-style reference from the n-th entry of the day for a
// prefix. The sequence is scrambled by an odd multiplier modulo 2^25 — a bijection — so references
// are unique (up to 33.5 million per prefix per day) without looking sequential.
func SequencedTransactionReference(c EntryCategory, bookingDate Date, sequence int64) (string, error) {
	return sequencedReference(TransactionReferencePrefix(c), bookingDate, sequence)
}

func sequencedReference(prefix string, bookingDate Date, sequence int64) (string, error) {
	if sequence < 0 || sequence >= referenceSpace {
		return "", fmt.Errorf("transaction reference space exhausted for %s on %s", prefix, bookingDate)
	}
	scrambled := (sequence * 0x1E3779B1) & (referenceSpace - 1)
	suffix := make([]byte, 5)
	for i := 4; i >= 0; i-- {
		suffix[i] = referenceAlphabet[scrambled&31]
		scrambled >>= 5
	}
	return fmt.Sprintf("%s%02d%03d%s", prefix, bookingDate.Y%100, bookingDate.YearDay(), suffix), nil
}

type leg struct {
	account   *LedgerAccount
	direction EntryDirection
	amount    int64
}

// EntryBuilder builds and posts a journal entry, enforcing the ledger's invariants:
//   - at least two legs, all with positive amounts;
//   - debits equal credits per currency (multi-currency entries balance in each leg currency);
//   - accounts that disallow negative balances are never overdrawn.
//
// All checks run before any balance is mutated, so a rejected posting leaves no trace.
type EntryBuilder struct {
	Reference   string
	category    EntryCategory
	description string
	legs        []leg
	sequence    *int64
	err         error
}

// Category is the entry's business category.
func (b *EntryBuilder) Category() EntryCategory { return b.category }

// Sequence makes the transaction reference derive from the n-th entry of the day for this
// category's prefix (see SequencedTransactionReference) instead of a random suffix.
func (b *EntryBuilder) Sequence(n int64) *EntryBuilder {
	b.sequence = &n
	return b
}

func NewEntry(reference string, category EntryCategory, description string) *EntryBuilder {
	return &EntryBuilder{Reference: reference, category: category, description: description}
}

func (b *EntryBuilder) Debit(a *LedgerAccount, amount int64) *EntryBuilder {
	return b.Add(a, Debit, amount)
}

func (b *EntryBuilder) Credit(a *LedgerAccount, amount int64) *EntryBuilder {
	return b.Add(a, Credit, amount)
}

// DebitSigned adds a leg whose direction follows the sign: positive debits, negative credits.
func (b *EntryBuilder) DebitSigned(a *LedgerAccount, signed int64) *EntryBuilder {
	if signed >= 0 {
		return b.Debit(a, signed)
	}
	return b.Credit(a, -signed)
}

func (b *EntryBuilder) Add(a *LedgerAccount, direction EntryDirection, amount int64) *EntryBuilder {
	if amount < 0 && b.err == nil {
		b.err = Rule("invalid_amount", "Journal line amounts must be positive; use the opposite direction instead.")
	}
	if amount > 0 { // zero legs (e.g. a zero fee) are dropped rather than persisted
		b.legs = append(b.legs, leg{a, direction, amount})
	}
	return b
}

func (b *EntryBuilder) HasLegs() bool { return len(b.legs) > 0 }

// Delta is the entry's net effect on an account's balance.
func (b *EntryBuilder) Delta(a *LedgerAccount) int64 {
	var d int64
	for _, l := range b.legs {
		if l.account == a {
			d += a.SignedDelta(l.direction, l.amount)
		}
	}
	return d
}

// Accounts returns the distinct accounts touched by the entry, in first-use order.
// Replace moves the legs on one account to another (a part of a split account).
func (b *EntryBuilder) Replace(from, to *LedgerAccount) {
	for i := range b.legs {
		if b.legs[i].account == from {
			b.legs[i].account = to
		}
	}
}

func (b *EntryBuilder) Accounts() []*LedgerAccount {
	var out []*LedgerAccount
	seen := map[*LedgerAccount]bool{}
	for _, l := range b.legs {
		if !seen[l.account] {
			seen[l.account] = true
			out = append(out, l.account)
		}
	}
	return out
}

// Post validates the entry and applies it to the accounts' balances. overdraftPermitted lists
// accounts allowed to go negative for this posting only — e.g. a merchant debited for a
// chargeback it cannot currently cover.
func (b *EntryBuilder) Post(now time.Time, bookingDate Date, overdraftPermitted ...*LedgerAccount) (*JournalEntry, error) {
	if b.err != nil {
		return nil, b.err
	}
	if b.Reference == "" {
		return nil, Rule("invalid_entry", "A journal entry requires a reference.")
	}
	if len(b.legs) < 2 {
		return nil, Rule("invalid_entry", "A journal entry requires at least two non-zero lines.")
	}

	totals := map[string][2]int64{}
	for _, l := range b.legs {
		t := totals[l.account.Currency]
		if l.direction == Debit {
			t[0] += l.amount
		} else {
			t[1] += l.amount
		}
		totals[l.account.Currency] = t
	}
	currencies := make([]string, 0, len(totals))
	for c := range totals {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		if t := totals[c]; t[0] != t[1] {
			return nil, Rule("unbalanced_entry", "Entry '%s' is unbalanced in %s: debits %d != credits %d.", b.Reference, c, t[0], t[1])
		}
	}

	// Dry-run the balance effect per account before mutating anything.
	for _, a := range b.Accounts() {
		projected := a.Balance
		for _, l := range b.legs {
			if l.account == a {
				projected += a.SignedDelta(l.direction, l.amount)
			}
		}
		mayOverdraw := a.AllowNegative
		for _, o := range overdraftPermitted {
			mayOverdraw = mayOverdraw || o == a
		}
		if projected < 0 && !mayOverdraw {
			return nil, Rule("insufficient_funds", "Account %s has insufficient funds.", a.AccountNumber)
		}
	}

	reference := NewTransactionReference(b.category, bookingDate)
	if b.sequence != nil {
		var err error
		if reference, err = SequencedTransactionReference(b.category, bookingDate, *b.sequence); err != nil {
			return nil, err
		}
	}
	entry := &JournalEntry{
		ID:                   NewID(),
		Reference:            b.Reference,
		TransactionReference: reference,
		Category:             b.category,
		Description:          b.description,
		PostedAt:             now,
		BookingDate:          bookingDate,
		ValueDate:            bookingDate,
		HashVersion:          2,
	}
	for _, l := range b.legs {
		l.account.Balance += l.account.SignedDelta(l.direction, l.amount)
		entry.Lines = append(entry.Lines, JournalLine{
			ID: NewID(), JournalEntryID: entry.ID, AccountID: l.account.ID, Direction: l.direction,
			Amount: l.amount, Currency: l.account.Currency, BalanceAfter: l.account.Balance, BookingDate: bookingDate,
		})
	}
	return entry, nil
}

// SystemAccountSpec defines a platform-owned (internal or P&L) ledger account, keyed by a deterministic code.
type SystemAccountSpec struct {
	Code          string
	Name          string
	Type          AccountType
	Category      int
	Currency      string
	AllowNegative bool
}

func spec(code, name string, typ AccountType, category int, currency string) SystemAccountSpec {
	return SystemAccountSpec{code, name, typ, category, currency, true}
}

// ProviderFloat holds funds at (or owed by) a PSP / mobile-money float for a provider.
func ProviderFloat(provider, currency string) SystemAccountSpec {
	return spec("asset:provider:"+provider+":"+currency, provider+" float", Asset, CategoryProviderFloat, currency)
}

// SettlementBank is the platform operating bank account that receives PSP settlements.
func SettlementBank(currency string) SystemAccountSpec {
	return spec("asset:bank:settlement:"+currency, "Settlement bank", Asset, CategorySettlementBank, currency)
}

// Suspense holds unidentified or disputed items. It must be cleared daily.
func Suspense(currency string) SystemAccountSpec {
	return spec("asset:suspense:"+currency, "Suspense", Asset, CategorySuspense, currency)
}

func FeeRevenue(currency string) SystemAccountSpec {
	return spec("revenue:fees:"+currency, "Fee income", Revenue, CategoryFeeIncome, currency)
}

func FxRevenue(currency string) SystemAccountSpec {
	return spec("revenue:fx:"+currency, "FX income", Revenue, CategoryFxIncome, currency)
}

// FxPosition is the platform FX inventory; it goes long/short per currency as customers convert.
func FxPosition(currency string) SystemAccountSpec {
	return spec("equity:fx-position:"+currency, "FX position", Equity, CategoryFxPosition, currency)
}

// PayoutsInTransit holds customer funds reserved for payouts the rail has not yet confirmed.
func PayoutsInTransit(currency string) SystemAccountSpec {
	return spec("liability:payouts-in-transit:"+currency, "Payouts in transit", Liability, CategoryPayoutsInTransit, currency)
}

// CardSchemePayable holds captured card spend owed to the card scheme until scheme settlement.
func CardSchemePayable(currency string) SystemAccountSpec {
	return spec("liability:card-scheme-payable:"+currency, "Card scheme payable", Liability, CategoryCardSchemePayable, currency)
}

// InterestPayable holds interest accrued by COB but not yet capitalised to customer accounts.
func InterestPayable(currency string) SystemAccountSpec {
	return spec("liability:interest-payable:"+currency, "Interest payable", Liability, CategoryInterestPayable, currency)
}

func ProviderFees(currency string) SystemAccountSpec {
	return spec("expense:provider-fees:"+currency, "Processing costs", Expense, CategoryProcessingCosts, currency)
}

// SuspenseWriteOff is the P&L account suspense items age into when nobody can claim them.
func SuspenseWriteOff(currency string) SystemAccountSpec {
	return spec("expense:suspense-write-off:"+currency, "Suspense write-offs", Expense, CategorySuspenseWriteOff, currency)
}

func InterestExpense(currency string) SystemAccountSpec {
	return spec("expense:interest:"+currency, "Interest expense", Expense, CategoryInterestExpense, currency)
}

func WalletAvailableCode(walletID uuid.UUID) string {
	return "liability:wallet:" + N(walletID) + ":available"
}
func WalletHeldCode(walletID uuid.UUID) string { return "liability:wallet:" + N(walletID) + ":held" }

// AllSystemAccounts lists every platform account that must exist for a currency, given the configured providers.
func AllSystemAccounts(currency string, providers []string) []SystemAccountSpec {
	specs := []SystemAccountSpec{
		SettlementBank(currency), Suspense(currency), FeeRevenue(currency), FxRevenue(currency), FxPosition(currency),
		PayoutsInTransit(currency), CardSchemePayable(currency), InterestPayable(currency), UnappliedFunds(currency), ProviderFees(currency), InterestExpense(currency), SuspenseWriteOff(currency),
	}
	for _, p := range providers {
		specs = append(specs, ProviderFloat(p, currency))
	}
	return specs
}

// ComputeHash is the entry's content hash: header fields plus every line in posting order.
func (e *JournalEntry) ComputeHash() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s|%s", e.Reference, e.TransactionReference, e.Category, e.Description,
		e.PostedAt.UTC().Format(time.RFC3339Nano), e.BookingDate, e.ValueDate)
	for _, l := range e.Lines {
		if e.HashVersion >= 2 {
			fmt.Fprintf(&b, "\n%s|%s|%d|%s|%s", l.AccountID, l.Direction, l.Amount, l.Currency, l.BookingDate)
			continue
		}
		fmt.Fprintf(&b, "\n%s|%s|%d|%s|%d|%s", l.AccountID, l.Direction, l.Amount, l.Currency, l.BalanceAfter, l.BookingDate)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
