package domain

import (
	"database/sql/driver"
	"slices"
	"time"
)

// ---------------------------------------------------------------------------------------------
// Provider modules: what a provider may do (see pkg/provapi)

// HookProvider is the hook point a provider module's endpoint serves: the provider protocol.
const HookProvider = "provider"

// ProviderCapabilities are what a provider may do, as its module described them when the
// provider's channel was onboarded or last amended. A checker authorised exactly this; the core
// uses nothing else, whatever the module describes later.
type ProviderCapabilities struct {
	Name                   string               `json:"name"`
	Collections            []ProviderCollection `json:"collections,omitempty"`
	Payouts                []ProviderPayoutRail `json:"payouts,omitempty"`
	Deposits               bool                 `json:"deposits,omitempty"`
	BankCode               string               `json:"bankCode,omitempty"`
	NotificationValidation string               `json:"notificationValidation,omitempty"`
	SettlementConfirmed    bool                 `json:"settlementConfirmed,omitempty"`
	CheckoutExpirySeconds  int                  `json:"checkoutExpirySeconds,omitempty"`
	IdempotentCharges      bool                 `json:"idempotentCharges,omitempty"`
	Actions                []ProviderAction     `json:"actions,omitempty"`
	CheckCharge            bool                 `json:"checkCharge,omitempty"`
	CheckPayout            bool                 `json:"checkPayout,omitempty"`
	CheckTransaction       bool                 `json:"checkTransaction,omitempty"`
	RequestStatus          bool                 `json:"requestStatus,omitempty"`
	Balance                bool                 `json:"balance,omitempty"`
	Webhooks               bool                 `json:"webhooks,omitempty"`
	// Details are the payment details the module returns; they are the columns of the
	// provider's details table (pd_<provider>).
	Details []ProviderDetail `json:"details,omitempty"`
}

// ProviderDetail is one payment detail a provider's module returns.
type ProviderDetail struct {
	Name       string         `json:"name"`
	Label      string         `json:"label,omitempty"`
	Type       LocalFieldType `json:"type,omitempty"`
	Searchable bool           `json:"searchable,omitempty"`
}

// DetailColumns turns the declared details into the details table's fields.
func (c ProviderCapabilities) DetailColumns() LocalColumns {
	out := make(LocalColumns, 0, len(c.Details))
	for _, d := range c.Details {
		f := LocalColumn{Name: d.Name, Label: d.Label, Type: d.Type, Searchable: d.Searchable}
		if f.Type == FieldText || f.Type == "" {
			f.Type, f.MaxLength = FieldText, 255
		}
		out = append(out, f)
	}
	return out
}

type ProviderCollection struct {
	Method     PaymentMethod `json:"method"`
	Currencies []string      `json:"currencies"`
	Hosted     bool          `json:"hosted,omitempty"`
}

type ProviderPayoutRail struct {
	Rail       PayoutRail `json:"rail"`
	Currencies []string   `json:"currencies"`
}

type ProviderAction struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func (c ProviderCapabilities) Value() (driver.Value, error) { return jsonValue(c) }
func (c *ProviderCapabilities) Scan(src any) error          { return jsonScan(src, c) }

// Collects reports whether the provider collects a method in a currency, and whether the payer
// pays on the provider's own page.
func (c ProviderCapabilities) Collects(method PaymentMethod, currency string) (ok, hosted bool) {
	for _, col := range c.Collections {
		if col.Method == method && slices.Contains(col.Currencies, currency) {
			return true, col.Hosted
		}
	}
	return false, false
}

// Hosts reports whether any of the provider's collections is a hosted checkout.
func (c ProviderCapabilities) Hosts() bool {
	return slices.ContainsFunc(c.Collections, func(col ProviderCollection) bool { return col.Hosted })
}

// Pays reports whether the provider pays out on a rail in a currency.
func (c ProviderCapabilities) Pays(rail PayoutRail, currency string) bool {
	return slices.ContainsFunc(c.Payouts, func(p ProviderPayoutRail) bool { return p.Rail == rail && slices.Contains(p.Currencies, currency) })
}

// Offers reports whether an operator action is available.
func (c ProviderCapabilities) Offers(action string) bool {
	return slices.ContainsFunc(c.Actions, func(a ProviderAction) bool { return a.Name == action })
}

// CheckoutExpiry is how long an unfinished hosted checkout stays open.
func (c ProviderCapabilities) CheckoutExpiry() time.Duration {
	if c.CheckoutExpirySeconds <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.CheckoutExpirySeconds) * time.Second
}

// Currencies lists every currency the provider moves, sorted.
func (c ProviderCapabilities) Currencies() []string {
	var out []string
	for _, col := range c.Collections {
		out = append(out, col.Currencies...)
	}
	for _, p := range c.Payouts {
		out = append(out, p.Currencies...)
	}
	return uniqueSorted(out)
}

// Normalize validates a description from a module: known methods, rails and currencies only.
func (c *ProviderCapabilities) Normalize() error {
	for i, col := range c.Collections {
		method, ok := ParseEnum(string(col.Method), MethodCard, MethodMobileMoney)
		if !ok {
			return Rule("invalid_capabilities", "Unknown payment method '%s'.", col.Method)
		}
		currencies, err := normalizeCurrencies(col.Currencies)
		if err != nil {
			return err
		}
		c.Collections[i] = ProviderCollection{Method: method, Currencies: currencies, Hosted: col.Hosted}
	}
	for i, p := range c.Payouts {
		rail, ok := ParseEnum(string(p.Rail), RailMobileMoney, RailBank)
		if !ok {
			return Rule("invalid_capabilities", "Unknown payout rail '%s'.", p.Rail)
		}
		currencies, err := normalizeCurrencies(p.Currencies)
		if err != nil {
			return err
		}
		c.Payouts[i] = ProviderPayoutRail{Rail: rail, Currencies: currencies}
	}
	if len(c.Currencies()) == 0 {
		return Rule("invalid_capabilities", "The provider neither collects nor pays out in any currency.")
	}
	if c.Hosts() && !c.CheckTransaction {
		return Rule("invalid_capabilities", "A hosted checkout must let the core confirm results with the provider (check_transaction).")
	}
	if c.CheckoutExpirySeconds < 0 || c.CheckoutExpirySeconds > 24*60*60 {
		return Rule("invalid_capabilities", "A checkout expires within a day.")
	}
	for i, d := range c.Details {
		switch d.Type {
		case "":
			c.Details[i].Type = FieldText
		case FieldText, FieldNumber, FieldAmount, FieldDate:
		default:
			return Rule("invalid_capabilities", "Payment detail '%s' has type '%s'; it must be text, number, amount or date.", d.Name, d.Type)
		}
	}
	spec := LocalTableSpec{Kind: LocalDetails, Description: "details", Fields: c.DetailColumns()}
	if err := spec.Normalize(); err != nil {
		return Rule("invalid_capabilities", "Payment details: %s", err.Error())
	}
	return nil
}

func normalizeCurrencies(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, code := range in {
		ccy, err := CurrencyOf(code)
		if err != nil {
			return nil, err
		}
		out = append(out, ccy.Code)
	}
	if len(out) == 0 {
		return nil, Rule("invalid_capabilities", "A capability needs at least one currency.")
	}
	return uniqueSorted(out), nil
}

// CapabilityChanges describes, for a checker, how new capabilities differ from the current ones.
func CapabilityChanges(from, to ProviderCapabilities) []string {
	var changes []string
	describe := func(c ProviderCapabilities) map[string]bool {
		set := map[string]bool{}
		for _, col := range c.Collections {
			for _, ccy := range col.Currencies {
				set["collects "+string(col.Method)+" "+ccy+map[bool]string{true: " (hosted)"}[col.Hosted]] = true
			}
		}
		for _, p := range c.Payouts {
			for _, ccy := range p.Currencies {
				set["pays out "+string(p.Rail)+" "+ccy] = true
			}
		}
		for flag, on := range map[string]bool{"deposit notifications": c.Deposits, "settlement-confirmed collections": c.SettlementConfirmed, "idempotent charges": c.IdempotentCharges,
			"charge status checks": c.CheckCharge, "payout status checks": c.CheckPayout, "transaction checks": c.CheckTransaction, "status queries": c.RequestStatus, "balance enquiry": c.Balance} {
			if on {
				set[flag] = true
			}
		}
		for _, a := range c.Actions {
			set["action "+a.Name] = true
		}
		if c.BankCode != "" {
			set["bank code "+c.BankCode] = true
		}
		for _, d := range c.Details {
			set["payment detail "+d.Name] = true
		}
		return set
	}
	before, after := describe(from), describe(to)
	for k := range after {
		if !before[k] {
			changes = append(changes, "+ "+k)
		}
	}
	for k := range before {
		if !after[k] {
			changes = append(changes, "- "+k)
		}
	}
	slices.Sort(changes)
	return changes
}
