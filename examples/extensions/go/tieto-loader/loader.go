// Package tietoloader is the Tieto prepaid-card module as an in-process Go extension. Onboard
// the TIETO channel with extension "tieto-loader", register the module in
// cmd/zirnis/extensions.go:
//
//	sys.App.Extensions.MustRegister(tietoloader.New(tietoloader.Simulator{}))
//
// and every card load (POST /channels/TIETO/transfers) debits the customer into TIETOOUT and
// calls OnFunds, which loads the card and answers complete, return or hold.
package tietoloader

import (
	"context"
	"fmt"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

// CardAPI is Tieto's card-load API. idempotencyKey is the suspense item id: a retried event must
// return the first load's result, never load the card twice.
type CardAPI interface {
	Load(ctx context.Context, cardToken string, amount int64, currency, idempotencyKey string) (status, reference string, err error)
}

type Loader struct{ api CardAPI }

func New(api CardAPI) *Loader { return &Loader{api} }

func (l *Loader) Name() string { return "tieto-loader" }

func (l *Loader) OnFunds(ctx context.Context, e extapi.FundsEvent) (extapi.FundsOutcome, error) {
	if e.Channel == nil || e.Channel.Direction != "Outward" {
		return extapi.FundsOutcome{}, nil
	}
	status, reference, err := l.api.Load(ctx, e.Item.Attributes["card_token"], e.Item.OpenAmount, e.Item.Currency, e.Item.ID)
	if err != nil {
		return extapi.FundsOutcome{}, err // the core retries with backoff; the money waits in TIETOOUT
	}
	switch status {
	case "LOADED":
		return extapi.FundsOutcome{Action: extapi.ActionComplete, ProviderReference: reference}, nil
	case "REJECTED":
		return extapi.FundsOutcome{Action: extapi.ActionReturn, ProviderReference: reference, Narrative: "Card load rejected by Tieto"}, nil
	}
	return extapi.FundsOutcome{Action: extapi.ActionHold, ProviderReference: reference}, nil
}

// Simulator stands in for Tieto: tokens starting "blocked" are rejected.
type Simulator struct{}

func (Simulator) Load(_ context.Context, cardToken string, _ int64, _, key string) (string, string, error) {
	if len(cardToken) >= 7 && cardToken[:7] == "blocked" {
		return "REJECTED", "SIM-REJ-" + key[:8], nil
	}
	return "LOADED", fmt.Sprintf("SIM-%s", key[:8]), nil
}
