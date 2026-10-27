// Package verto is the Verto provider module (cross-border payouts and collections) as an
// in-process Go extension: the core's VERTO channel holds the money, this module talks to
// Verto. See docs/guides/adding-a-provider.md.
//
// Outward (a customer pays someone abroad): POST /channels/VERTO/transfers debits the customer
// into VERTOOUT and calls OnFunds; the module creates the Verto payout and answers hold, and
// Verto's webhook later settles the item (completed → delivered, archived/refunded → the
// customer is refunded). Inward (money arrives in a Verto wallet or sub-account): Verto's
// webhook books it into VERTOIN through the core's signed callback, credited to the customer
// the money is for.
//
// Register it in cmd/zirnis/extensions.go:
//
//	sys.App.Extensions.MustRegister(verto.New(api))
package verto

import (
	"context"
	"fmt"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Payout is a Verto payment as the module needs it.
type Payout struct {
	ID    string // Verto's payment id
	State string // requested | completed | archived | refunded
}

// PayoutRequest pays a beneficiary already created in Verto (POST /recipients on the
// beneficiary service) from the Verto wallet in the payout currency.
type PayoutRequest struct {
	ClientReference string // the suspense item id: our idempotency key with Verto
	RecipientID     string
	Amount          int64 // minor units
	Currency        string
	Purpose         string
}

// PayoutsAPI is Verto's payments service (api-payment-sandbox / api-payment-beta.vertofx.com,
// bearer token from POST {auth}/users/login). Verto documents no idempotency header, so a
// retried event must first look the payout up by its client reference and never pay twice.
type PayoutsAPI interface {
	FindByClientReference(ctx context.Context, clientReference string) (*Payout, error)
	CreatePayout(ctx context.Context, r PayoutRequest) (*Payout, error)
}

type Module struct{ api PayoutsAPI }

func New(api PayoutsAPI) *Module { return &Module{api} }

func (m *Module) Name() string { return "verto" }

func (m *Module) OnFunds(ctx context.Context, e extapi.FundsEvent) (extapi.FundsOutcome, error) {
	if e.Channel == nil || e.Channel.Direction != "Outward" {
		return extapi.FundsOutcome{}, nil // inward money is booked with its beneficiary by the webhook
	}
	item := e.Item
	payout, err := m.api.FindByClientReference(ctx, item.ID)
	if err != nil {
		return extapi.FundsOutcome{}, err // the core retries with backoff; the money waits in VERTOOUT
	}
	if payout == nil {
		recipient := item.Attributes["verto_recipient_id"]
		if recipient == "" {
			return extapi.FundsOutcome{Action: extapi.ActionReturn, Narrative: "No Verto beneficiary on the transfer"}, nil
		}
		if payout, err = m.api.CreatePayout(ctx, PayoutRequest{ClientReference: item.ID, RecipientID: recipient,
			Amount: item.OpenAmount, Currency: item.Currency, Purpose: item.Attributes["purpose_code"]}); err != nil {
			return extapi.FundsOutcome{}, err
		}
	}
	return Outcome(payout.State, payout.ID), nil
}

// Outcome maps a Verto payout state to the core's answer. "completed" means Verto sent the
// money (not that the beneficiary has it); "archived" and "refunded" mean it came back to the
// Verto wallet, so the customer is refunded.
func Outcome(state, verto string) extapi.FundsOutcome {
	switch state {
	case "completed":
		return extapi.FundsOutcome{Action: extapi.ActionComplete, ProviderReference: verto}
	case "archived", "refunded":
		return extapi.FundsOutcome{Action: extapi.ActionReturn, ProviderReference: verto, Narrative: fmt.Sprintf("Verto payout %s", state)}
	}
	return extapi.FundsOutcome{Action: extapi.ActionHold, ProviderReference: verto}
}
