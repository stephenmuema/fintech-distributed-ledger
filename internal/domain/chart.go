package domain

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// GLSection splits the GL into balance sheet and P&L.
type GLSection string

const (
	BalanceSheetSection  GLSection = "BalanceSheet"
	ProfitAndLossSection GLSection = "ProfitAndLoss"
)

// GLAccount is a general-ledger line (T24: RE.STAT.REP.LINE in the GL report structure).
type GLAccount struct {
	Code    string      `json:"code"`
	Name    string      `json:"name"`
	Type    AccountType `json:"type"`
	Section GLSection   `json:"section"`
	Group   string      `json:"group"`
}

// CategoryDefinition is a product / internal / P&L category (T24: CATEGORY) and the GL line it consolidates into.
type CategoryDefinition struct {
	Code   int    `json:"code"`
	Name   string `json:"name"`
	GLCode string `json:"glCode"`
}

// Categories follow T24 convention: 1000–9999 customer account products, 10000–19999 internal
// accounts, 50000–69999 profit & loss.
const (
	CategoryMerchantCurrentAccount = 1001
	CategoryVostro                 = 5001
	CategoryMobileWallet           = 6001

	CategoryProviderFloat     = 14001
	CategorySettlementBank    = 14002
	CategoryNostro            = 14003
	CategorySuspense          = 14009
	CategoryPayoutsInTransit  = 16001
	CategoryCardSchemePayable = 16002
	CategoryInterestPayable   = 16003
	CategoryUnappliedFunds    = 16004
	CategoryFxPosition        = 18001

	CategoryFeeIncome        = 52000
	CategoryFxIncome         = 52100
	CategoryProcessingCosts  = 60100
	CategoryInterestExpense  = 60200
	CategorySuspenseWriteOff = 60300
)

// GLAccounts is the chart of GL report lines.
var GLAccounts = []GLAccount{
	{"1100", "Cash at bank — settlement accounts", Asset, BalanceSheetSection, "Assets"},
	{"1150", "Due from correspondent banks — nostro", Asset, BalanceSheetSection, "Assets"},
	{"1200", "Due from PSPs and mobile-money floats", Asset, BalanceSheetSection, "Assets"},
	{"1900", "Suspense", Asset, BalanceSheetSection, "Assets"},
	{"1910", "Debit suspense", Asset, BalanceSheetSection, "Assets"},
	{"2100", "Customer deposits — wallets", Liability, BalanceSheetSection, "Liabilities"},
	{"2110", "Customer deposits — merchant accounts", Liability, BalanceSheetSection, "Liabilities"},
	{"2120", "Due to banks — vostro accounts", Liability, BalanceSheetSection, "Liabilities"},
	{"2200", "Payouts in transit", Liability, BalanceSheetSection, "Liabilities"},
	{"2300", "Card scheme payable", Liability, BalanceSheetSection, "Liabilities"},
	{"2400", "Interest payable", Liability, BalanceSheetSection, "Liabilities"},
	{"2500", "Unapplied customer receipts", Liability, BalanceSheetSection, "Liabilities"},
	{"2510", "Credit suspense", Liability, BalanceSheetSection, "Liabilities"},
	{"2520", "Provider inward transfers suspense", Liability, BalanceSheetSection, "Liabilities"},
	{"2530", "Provider outward transfers in transit", Liability, BalanceSheetSection, "Liabilities"},
	{"3100", "FX position", Equity, BalanceSheetSection, "Equity"},
	{"4100", "Fee and commission income", Revenue, ProfitAndLossSection, "Income"},
	{"4200", "Foreign exchange income", Revenue, ProfitAndLossSection, "Income"},
	{"5100", "Payment processing costs", Expense, ProfitAndLossSection, "Expenses"},
	{"5200", "Interest expense", Expense, ProfitAndLossSection, "Expenses"},
	{"5300", "Suspense write-offs", Expense, ProfitAndLossSection, "Expenses"},
}

// Categories maps every category to its GL line.
var Categories = []CategoryDefinition{
	{CategoryMerchantCurrentAccount, "Merchant current account", "2110"},
	{CategoryVostro, "Vostro — due to banks", "2120"},
	{CategoryMobileWallet, "Mobile wallet", "2100"},
	{CategoryProviderFloat, "PSP / MNO float", "1200"},
	{CategorySettlementBank, "Settlement bank", "1100"},
	{CategoryNostro, "Nostro account", "1150"},
	{CategorySuspense, "Suspense", "1900"},
	{CategoryPayoutsInTransit, "Payouts in transit", "2200"},
	{CategoryCardSchemePayable, "Card scheme payable", "2300"},
	{CategoryInterestPayable, "Interest payable", "2400"},
	{CategoryUnappliedFunds, "Unapplied receipts", "2500"},
	{CategoryFxPosition, "FX position", "3100"},
	{CategoryFeeIncome, "Fee income", "4100"},
	{CategoryFxIncome, "FX income", "4200"},
	{CategoryProcessingCosts, "Processing costs", "5100"},
	{CategoryInterestExpense, "Interest expense", "5200"},
	{CategorySuspenseWriteOff, "Suspense write-offs", "5300"},
}

// Category ranges allocated at runtime from data (suspense types are parameter records, so their
// categories are too).
const (
	SuspenseCategoryFirst = 14100
	SuspenseCategoryLast  = 14999
)

// GLFor returns the GL line a built-in category consolidates into. Categories allocated at
// runtime live in the database; resolve those through a Chart.
func GLFor(category int) (string, error) {
	return BuiltInChart().GLFor(category)
}

// Chart is the full chart of accounts: the built-in GL lines and categories plus the categories
// allocated at runtime (T24: CATEGORY records created by parameter maintenance).
type Chart struct {
	GLAccounts []GLAccount          `json:"glAccounts"`
	Categories []CategoryDefinition `json:"categories"`
}

// BuiltInChart is the chart as shipped.
func BuiltInChart() Chart {
	return Chart{GLAccounts: GLAccounts, Categories: Categories}
}

// WithCategories returns the chart extended by runtime categories.
func (c Chart) WithCategories(extra []CategoryDefinition) Chart {
	all := append(append([]CategoryDefinition{}, c.Categories...), extra...)
	sort.Slice(all, func(i, j int) bool { return all[i].Code < all[j].Code })
	return Chart{GLAccounts: c.GLAccounts, Categories: all}
}

// GLFor returns the GL line a category consolidates into.
func (c Chart) GLFor(category int) (string, error) {
	for _, d := range c.Categories {
		if d.Code == category {
			return d.GLCode, nil
		}
	}
	return "", Rule("invalid_category", "Category %d is not in the chart of accounts.", category)
}

// Category looks up a category definition.
func (c Chart) Category(code int) (CategoryDefinition, bool) {
	for _, d := range c.Categories {
		if d.Code == code {
			return d, true
		}
	}
	return CategoryDefinition{}, false
}

// NextFree returns the lowest unused category in [first, last].
func (c Chart) NextFree(first, last int) (int, error) {
	used := map[int]bool{}
	for _, d := range c.Categories {
		used[d.Code] = true
	}
	for code := first; code <= last; code++ {
		if !used[code] {
			return code, nil
		}
	}
	return 0, Rule("category_range_exhausted", "No free category between %d and %d.", first, last)
}

// RuntimeCategory is a category allocated by parameter maintenance (stored in categories).
type RuntimeCategory struct {
	ID        uuid.UUID `db:"id" json:"id"`
	Code      int       `db:"code" json:"code"`
	Name      string    `db:"name" json:"name"`
	GLCode    string    `db:"gl_code" json:"glCode"`
	Owner     string    `db:"owner" json:"owner"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
}

func (r RuntimeCategory) Definition() CategoryDefinition {
	return CategoryDefinition{Code: r.Code, Name: r.Name, GLCode: r.GLCode}
}

// GLAccountByCode looks up a GL line.
func GLAccountByCode(code string) (GLAccount, bool) {
	for _, g := range GLAccounts {
		if g.Code == code {
			return g, true
		}
	}
	return GLAccount{}, false
}

func IsProfitAndLoss(category int) bool { return category >= 50000 && category <= 69999 }

// InternalAccountNumber is the T24 layout {CCY}{CATEGORY}{SEQ:4}, e.g. KES140010001.
// P&L categories are not accounts in T24; they're addressed as PL{CATEGORY}{CCY}.
func InternalAccountNumber(category int, currency string, sequence int) string {
	if IsProfitAndLoss(category) {
		return fmt.Sprintf("PL%d%s", category, currency)
	}
	return fmt.Sprintf("%s%d%04d", currency, category, sequence)
}

// CustomerAccountNumber is a 10-digit number with a Luhn check digit, e.g. 1000000018.
func CustomerAccountNumber(sequence int64) string {
	body := fmt.Sprintf("1%08d", sequence)
	return body + string(rune('0'+luhnCheckDigit(body)))
}

func IsValidCustomerAccountNumber(number string) bool {
	if len(number) != 10 {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	return luhnCheckDigit(number[:9]) == int(number[9]-'0')
}

func luhnCheckDigit(digits string) int {
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return (10 - sum%10) % 10
}
