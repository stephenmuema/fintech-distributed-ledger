package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// txnFlow executes one flow. prepare and overrides only read; execute runs the core service
// under the transaction's binding.
type txnFlow interface {
	// prepare checks the input against current state and fills the record's accounts and
	// parties, the amount and the currency.
	prepare(a *App, q store.Q, p *prepared) error
	// overrides are the warnings the input raises (the version classes them).
	overrides(a *App, q store.Q, p *prepared) ([]domain.Override, error)
	// execute moves the money and returns the flow record.
	execute(ctx context.Context, a *App, p *prepared) (any, error)
}

var flows = map[domain.TransactionFlow]txnFlow{
	domain.FlowTransfer:   transferFlow{},
	domain.FlowCollection: collectionFlow{},
	domain.FlowPayout:     payoutFlow{},
	domain.FlowRefund:     refundFlow{},
	domain.FlowChannelOut: channelOutFlow{},
}

// HighValueThresholdUSD raises the HIGH.VALUE override.
var HighValueThresholdUSD = decimal.NewFromInt(5_000)

func requireField(p *prepared, path, label string) (string, error) {
	v := p.field(path)
	if v == "" {
		return "", domain.Rule("field_required", "%s is required.", label)
	}
	return v, nil
}

func requireAmount(p *prepared) error {
	if p.amount <= 0 {
		return domain.Rule("field_required", "Amount is required.")
	}
	return nil
}

// account loads a customer account by number.
func account(a *App, q store.Q, number, label string) (*domain.Wallet, error) {
	if !domain.IsValidCustomerAccountNumber(number) {
		return nil, domain.Rule("invalid_account_number", "%s '%s' is not a valid account number.", label, number)
	}
	return a.Accounts.loadByNumber(q, number)
}

// holder fills a party from an account and its customer.
func holder(a *App, q store.Q, w *domain.Wallet) (id *uuid.UUID, name, phone *string, err error) {
	c, err := a.Customers.load(q, w.CustomerID)
	if err != nil {
		return nil, nil, nil, err
	}
	return &c.ID, &c.LegalName, c.Phone, nil
}

func (p *prepared) debitFrom(a *App, q store.Q, w *domain.Wallet) error {
	id, name, phone, err := holder(a, q, w)
	if err != nil {
		return err
	}
	r := p.record
	r.DebitAccount, r.DebitCurrency, r.DebitAmount = &w.AccountNumber, &w.Currency, domain.Ptr(p.amount)
	r.OrderingCustomerID, r.OrderingName, r.OrderingAccount, r.OrderingPhone = id, name, &w.AccountNumber, phone
	p.debit = w
	return nil
}

func (p *prepared) creditTo(a *App, q store.Q, w *domain.Wallet) error {
	id, name, phone, err := holder(a, q, w)
	if err != nil {
		return err
	}
	r := p.record
	r.CreditAccount, r.CreditCurrency, r.CreditAmount = &w.AccountNumber, &w.Currency, domain.Ptr(p.amount)
	r.BeneficiaryCustomerID, r.BeneficiaryName, r.BeneficiaryAccount, r.BeneficiaryPhone = id, name, &w.AccountNumber, phone
	p.credit = w
	return nil
}

func highValue(a *App, amount int64, currency string) ([]domain.Override, error) {
	usd, err := a.toUSD(amount, currency)
	if err != nil {
		return nil, err
	}
	if usd.LessThan(HighValueThresholdUSD) {
		return nil, nil
	}
	return []domain.Override{{Code: "HIGH.VALUE", Message: fmt.Sprintf("Amount %s is above the high-value threshold.", domain.MustCurrency(currency).Format(amount))}}, nil
}

// ---------------------------------------------------------------------------------------------
// transfer — book transfer between two customer accounts (T24: FT type AC)

type transferFlow struct{}

func (transferFlow) prepare(a *App, q store.Q, p *prepared) error {
	debitNumber, err := requireField(p, "debitAccount", "Debit account")
	if err != nil {
		return err
	}
	creditNumber, err := requireField(p, "creditAccount", "Credit account")
	if err != nil {
		return err
	}
	if err := requireAmount(p); err != nil {
		return err
	}
	debit, err := account(a, q, debitNumber, "Debit account")
	if err != nil {
		return err
	}
	credit, err := account(a, q, creditNumber, "Credit account")
	if err != nil {
		return err
	}
	if debit.ID == credit.ID {
		return domain.Rule("same_account", "The debit and credit accounts are the same.")
	}
	if debit.Currency != credit.Currency {
		return domain.Rule("currency_mismatch", "Transfers require accounts in the same currency; use FX conversion first.")
	}
	p.currency = debit.Currency
	if err := p.debitFrom(a, q, debit); err != nil {
		return err
	}
	return p.creditTo(a, q, credit)
}

func (transferFlow) overrides(a *App, q store.Q, p *prepared) ([]domain.Override, error) {
	overrides, err := highValue(a, p.amount, p.currency)
	if err != nil {
		return nil, err
	}
	known, err := store.Exists(q, "SELECT 1 FROM transfers WHERE source_wallet_id = ? AND destination_wallet_id = ? AND status = ?",
		p.debit.ID, p.credit.ID, domain.TransferCompleted)
	if err != nil {
		return nil, err
	}
	if !known {
		overrides = append(overrides, domain.Override{Code: "NEW.BENEFICIARY",
			Message: fmt.Sprintf("First transfer from %s to %s.", p.debit.AccountNumber, p.credit.AccountNumber)})
	}
	if p.debit.CustomerID != p.credit.CustomerID && p.credit.Restriction != domain.RestrictNone {
		overrides = append(overrides, domain.Override{Code: "BENEFICIARY.RESTRICTED",
			Message: fmt.Sprintf("Beneficiary account has restriction %s.", p.credit.Restriction)})
	}
	return overrides, nil
}

func (transferFlow) execute(ctx context.Context, a *App, p *prepared) (any, error) {
	return a.Transfers.Create(ctx, CreateTransfer{p.debit.ID, p.credit.ID, p.amount, p.record.Narrative})
}

// ---------------------------------------------------------------------------------------------
// collection — collect from a payer through a provider, into a merchant account

type collectionFlow struct{}

func (collectionFlow) prepare(a *App, q store.Q, p *prepared) error {
	var merchant *domain.Wallet
	var err error
	if code := p.field("linkCode"); code != "" {
		link, err := a.Links.loadByCode(q, code)
		if err != nil {
			return err
		}
		var requested *int64
		if p.amount > 0 {
			requested = &p.amount
		}
		if p.amount, err = link.ResolveAmount(requested, a.now()); err != nil {
			return err
		}
		if merchant, err = a.Accounts.loadWallet(q, link.MerchantWalletID); err != nil {
			return err
		}
		p.extra["link"] = link
	} else {
		number, err := requireField(p, "creditAccount", "Credit account")
		if err != nil {
			return err
		}
		if merchant, err = account(a, q, number, "Credit account"); err != nil {
			return err
		}
	}
	if err := requireAmount(p); err != nil {
		return err
	}
	p.currency = strings.ToUpper(firstNonEmpty(p.field("currency"), merchant.Currency))
	if p.currency != merchant.Currency {
		return domain.Rule("currency_mismatch", "Account %s is %s; the payment is %s.", merchant.AccountNumber, merchant.Currency, p.currency)
	}
	method, ok := domain.ParseEnum(firstNonEmpty(p.field("method"), string(domain.MethodCard)), domain.MethodCard, domain.MethodMobileMoney)
	if !ok {
		return domain.Rule("invalid_method", "Method must be Card or MobileMoney.")
	}
	source, err := requireField(p, "source", "Source (card token, MSISDN or hosted_checkout)")
	if err != nil {
		return err
	}
	p.extra["method"], p.extra["source"] = method, source
	if err := p.creditTo(a, q, merchant); err != nil {
		return err
	}
	r := p.record
	if payer := p.payer(); payer != nil {
		p.extra["payer"] = payer
		r.OrderingName = optional(strings.TrimSpace(payer.FirstName + " " + payer.LastName))
		r.OrderingPhone = optional(payer.Phone)
	}
	if method == domain.MethodMobileMoney && r.OrderingPhone == nil {
		r.OrderingPhone = &source
	}
	return nil
}

// payer is the hosted-checkout payer, when any payer field was input.
func (p *prepared) payer() *CheckoutCustomer {
	c := CheckoutCustomer{FirstName: p.field("payer.firstName"), LastName: p.field("payer.lastName"), Email: p.field("payer.email"),
		Phone: p.field("payer.phone"), Address: p.field("payer.address"), PostalCode: p.field("payer.postalCode")}
	if c == (CheckoutCustomer{}) {
		return nil
	}
	return &c
}

func (collectionFlow) overrides(*App, store.Q, *prepared) ([]domain.Override, error) { return nil, nil }

func (collectionFlow) execute(ctx context.Context, a *App, p *prepared) (any, error) {
	method, source := p.extra["method"].(domain.PaymentMethod), p.extra["source"].(string)
	payer, _ := p.extra["payer"].(*CheckoutCustomer)
	if link, ok := p.extra["link"].(*domain.PaymentLink); ok {
		return a.Payments.PayLink(ctx, link.Code, PayLink{Amount: &p.amount, Method: method, Source: source, Customer: payer})
	}
	return a.Payments.Create(ctx, CreatePayment{WalletID: p.credit.ID, Amount: p.amount, Currency: p.currency, Method: method, Source: source,
		Description: p.record.Narrative, Customer: payer, ReturnURL: p.optional("returnUrl")})
}

// ---------------------------------------------------------------------------------------------
// payout — pay out to a bank account or a mobile wallet

type payoutFlow struct{}

func (payoutFlow) prepare(a *App, q store.Q, p *prepared) error {
	number, err := requireField(p, "debitAccount", "Debit account")
	if err != nil {
		return err
	}
	if err := requireAmount(p); err != nil {
		return err
	}
	w, err := account(a, q, number, "Debit account")
	if err != nil {
		return err
	}
	rail, ok := domain.ParseEnum(firstNonEmpty(p.field("rail"), string(domain.RailMobileMoney)), domain.RailMobileMoney, domain.RailBank)
	if !ok {
		return domain.Rule("invalid_rail", "Rail must be MobileMoney or Bank.")
	}
	destination, err := domain.NewPayoutDestination(rail, p.field("beneficiary.account"), p.field("beneficiary.name"), p.optional("beneficiary.bankCode"))
	if err != nil {
		return err
	}
	p.currency = w.Currency
	if err := p.debitFrom(a, q, w); err != nil {
		return err
	}
	p.extra["destination"] = destination
	r := p.record
	r.Rail = domain.Ptr(string(rail))
	r.BeneficiaryName, r.BeneficiaryAccount = &destination.AccountName, &destination.AccountNumber
	r.BeneficiaryInstitution = firstPtr(destination.BankCode, domain.Ptr(string(rail)))
	if rail == domain.RailMobileMoney {
		r.BeneficiaryPhone = &destination.AccountNumber
	}
	return nil
}

func (payoutFlow) overrides(a *App, q store.Q, p *prepared) ([]domain.Override, error) {
	overrides, err := highValue(a, p.amount, p.currency)
	if err != nil {
		return nil, err
	}
	destination := p.extra["destination"].(domain.PayoutDestination)
	known, err := store.Exists(q, "SELECT 1 FROM payouts WHERE wallet_id = ? AND destination_account = ? AND status = ?",
		p.debit.ID, destination.AccountNumber, domain.PayoutPaid)
	if err != nil {
		return nil, err
	}
	if !known {
		overrides = append(overrides, domain.Override{Code: "NEW.BENEFICIARY",
			Message: fmt.Sprintf("First payout from %s to %s.", p.debit.AccountNumber, mask(destination.AccountNumber))})
	}
	return overrides, nil
}

func (payoutFlow) execute(ctx context.Context, a *App, p *prepared) (any, error) {
	return a.Payouts.Create(ctx, CreatePayout{WalletID: p.debit.ID, Amount: p.amount, Destination: p.extra["destination"].(domain.PayoutDestination),
		Narration: p.record.Narrative})
}

// ---------------------------------------------------------------------------------------------
// refund — refund all or part of a collection, named by its transaction reference

type refundFlow struct{}

func (refundFlow) prepare(a *App, q store.Q, p *prepared) error {
	reference, err := requireField(p, "originalReference", "Original transaction reference")
	if err != nil {
		return err
	}
	original, err := a.Transactions.load(q, reference)
	if err != nil {
		return err
	}
	if original.Flow != domain.FlowCollection || original.FlowRecordID == nil {
		return domain.Rule("not_refundable", "%s is not a collection.", original.Reference)
	}
	payment, err := a.Payments.load(q, *original.FlowRecordID)
	if err != nil {
		return err
	}
	if payment.Status != domain.PaymentSucceeded {
		return domain.Rule("payment_not_refundable", "Only succeeded payments can be refunded.")
	}
	if p.amount == 0 {
		p.amount = payment.RefundableAmount()
	}
	p.currency = payment.Currency
	p.extra["payment"] = payment
	r := p.record
	r.DebitAccount, r.DebitCurrency, r.DebitAmount = original.CreditAccount, original.CreditCurrency, domain.Ptr(p.amount)
	r.OrderingCustomerID, r.OrderingName, r.OrderingAccount = original.BeneficiaryCustomerID, original.BeneficiaryName, original.CreditAccount
	r.BeneficiaryName, r.BeneficiaryPhone, r.BeneficiaryInstitution = original.OrderingName, original.OrderingPhone, original.Provider
	r.Provider = payment.Provider
	r.PaymentDetails = append(domain.StringList{"Refund of " + original.Reference}, r.PaymentDetails...)
	return nil
}

func (refundFlow) overrides(*App, store.Q, *prepared) ([]domain.Override, error) { return nil, nil }

func (refundFlow) execute(ctx context.Context, a *App, p *prepared) (any, error) {
	payment := p.extra["payment"].(*domain.PaymentIntent)
	return a.Refunds.Create(ctx, payment.ID, &p.amount, firstPtr(p.optional("reason"), p.record.Narrative))
}

// ---------------------------------------------------------------------------------------------
// channel_out — send a customer's money through a provider channel

type channelOutFlow struct{}

func (channelOutFlow) prepare(a *App, q store.Q, p *prepared) error {
	code, err := requireField(p, "channelCode", "Channel")
	if err != nil {
		return err
	}
	number, err := requireField(p, "debitAccount", "Debit account")
	if err != nil {
		return err
	}
	if err := requireAmount(p); err != nil {
		return err
	}
	ch, err := a.Channels.load(q, strings.ToUpper(code))
	if err != nil {
		return err
	}
	w, err := account(a, q, number, "Debit account")
	if err != nil {
		return err
	}
	p.currency = w.Currency
	if err := p.debitFrom(a, q, w); err != nil {
		return err
	}
	p.extra["channel"] = ch.Code
	r := p.record
	r.BeneficiaryInstitution, r.BeneficiaryAccount, r.Provider = &ch.Code, p.optional("reference"), &ch.Code
	return nil
}

func (channelOutFlow) overrides(a *App, _ store.Q, p *prepared) ([]domain.Override, error) {
	return highValue(a, p.amount, p.currency)
}

func (channelOutFlow) execute(ctx context.Context, a *App, p *prepared) (any, error) {
	return a.Channels.Outward(ctx, p.extra["channel"].(string), OutwardRequest{AccountID: p.debit.ID, Amount: p.amount,
		Reference: p.field("reference"), Narrative: derefOr(p.record.Narrative, ""), Attributes: p.in.Attributes})
}

// ---------------------------------------------------------------------------------------------
// Routines

// routineView is the transaction as an input or authorisation routine sees it.
func (p *prepared) routineView(inputter, authoriser string) extapi.Transaction {
	r := p.record
	party := func(id *uuid.UUID, name, account, phone, institution *string) extapi.Party {
		out := extapi.Party{Name: derefOr(name, ""), Account: derefOr(account, ""), Phone: derefOr(phone, ""), Institution: derefOr(institution, "")}
		if id != nil {
			out.CustomerID = id.String()
		}
		return out
	}
	fields := map[string]string{}
	for k, v := range p.in.Fields {
		if k != "payer.email" && k != "payer.address" && k != "payer.postalCode" {
			fields[k] = v
		}
	}
	local := map[string]string{}
	for k, v := range p.in.Local {
		local[k] = v
	}
	return extapi.Transaction{Reference: r.Reference, Version: p.version.Name, TransactionType: p.txnType.Code, Flow: string(p.flow),
		Channel: p.in.Channel, Amount: p.amount, Currency: p.currency, DebitAccount: derefOr(r.DebitAccount, ""), CreditAccount: derefOr(r.CreditAccount, ""),
		Ordering:    party(r.OrderingCustomerID, r.OrderingName, r.OrderingAccount, r.OrderingPhone, r.OrderingInstitution),
		Beneficiary: party(r.BeneficiaryCustomerID, r.BeneficiaryName, r.BeneficiaryAccount, r.BeneficiaryPhone, r.BeneficiaryInstitution),
		Narrative:   derefOr(r.Narrative, ""), PaymentDetails: r.PaymentDetails, ReportingClass: r.ReportingClass, Fields: fields, Local: local,
		AcceptedOverrides: p.in.AcceptedOverrides, Inputter: inputter, Authoriser: authoriser}
}

func routineSubject(p *prepared) string {
	if p.record.Reference != "" {
		return p.record.Reference
	}
	return "input:" + p.version.Name
}

// consultInput runs the version's input routines (never inside a unit of work); their
// overrides join the core's.
func (s *Transactions) consultInput(ctx context.Context, p *prepared) error {
	if len(p.version.InputRoutines) == 0 {
		return nil
	}
	if p.record.ID != uuid.Nil && p.record.Reference == "" {
		if t, err := store.Get[domain.Transaction](s.a.read(ctx), "SELECT * FROM transactions WHERE id = ?", p.record.ID); err == nil && t != nil {
			p.record.Reference = t.Reference
		}
	}
	inputter := store.Actor(ctx)
	if p.record.Reference != "" {
		if t, err := s.load(s.a.read(ctx), p.record.Reference); err == nil {
			inputter = t.Inputter
		}
	}
	raised, err := s.a.Extensions.ValidateTransaction(ctx, p.version.InputRoutines, routineSubject(p), p.routineView(inputter, ""))
	if err != nil {
		return err
	}
	p.overrides = append(p.overrides, raised...)
	return nil
}

// consultAuth runs the version's authorisation routines just before the flow executes.
func (s *Transactions) consultAuth(ctx context.Context, p *prepared) error {
	if len(p.version.AuthRoutines) == 0 {
		return nil
	}
	authoriser := store.Actor(ctx)
	inputter := authoriser
	if p.record.Reference != "" {
		if t, err := s.load(s.a.read(ctx), p.record.Reference); err == nil {
			inputter = t.Inputter
		}
	} else {
		authoriser = "system.stp"
	}
	return s.a.Extensions.AuthoriseTransaction(ctx, p.version.AuthRoutines, routineSubject(p), p.routineView(inputter, authoriser))
}

// amountField renders an amount for the field map.
func amountField(n int64) string { return strconv.FormatInt(n, 10) }
