package verto

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Event is the part of a Verto webhook the module acts on (the body's "payload" object). For
// collections Verto sends id, amount (major units), currency, state, type (e.g. iban_to_wallet),
// walletId, companyId, reference and clientReference. Check your subscription's topics in the
// Verto platform for the exact fields of payout events.
type Event struct {
	ID              string          `json:"id"`
	Amount          decimal.Decimal `json:"amount"`
	Currency        string          `json:"currency"`
	State           string          `json:"state"`
	Type            string          `json:"type"`
	WalletID        string          `json:"walletId"`
	Reference       string          `json:"reference"`
	ClientReference string          `json:"clientReference"`
}

// Core is what the module tells Zirnis: extapi.Client satisfies it (signed callbacks).
type Core interface {
	Inward(ctx context.Context, channel string, funds extapi.InwardFunds) (map[string]any, error)
	ReportOutcome(ctx context.Context, itemID string, outcome extapi.FundsOutcome) (map[string]any, error)
}

// Beneficiaries finds the Zirnis account inward money is for, e.g. from the Verto wallet or
// sub-account it arrived in ("" when unknown: the money waits in VERTOIN for operations).
type Beneficiaries func(e Event) string

// Webhook receives Verto's deliveries: payout state changes settle the outward item named by
// the client reference; completed collections are booked inward. Anything unverified is 401.
func Webhook(v *Verifier, core Core, beneficiary Beneficiaries, exponent map[string]int32, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.Verify(r.Header); err != nil {
			http.Error(w, "unverified", http.StatusUnauthorized)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		var body struct {
			Payload Event `json:"payload"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Payload.ID == "" {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		e := body.Payload
		ctx := r.Context()
		switch {
		case isCollection(e) && e.State == "completed": // screened and in the wallet; "requested" is still being screened
			exp, ok := exponent[strings.ToUpper(e.Currency)]
			if !ok {
				exp = 2
			}
			_, err = core.Inward(ctx, "VERTO", extapi.InwardFunds{Currency: strings.ToUpper(e.Currency), Amount: e.Amount.Shift(exp).IntPart(),
				ProviderReference: e.ID, Narrative: "Verto " + e.Type + " " + e.Reference, BeneficiaryAccountNumber: beneficiary(e)})
		case !isCollection(e) && e.ClientReference != "":
			outcome := Outcome(e.State, e.ID)
			if outcome.Action != extapi.ActionHold {
				_, err = core.ReportOutcome(ctx, e.ClientReference, outcome)
			}
		}
		if err != nil {
			log.Error("verto webhook not applied; Verto will retry", "event", e.ID, "error", err)
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// isCollection: money arriving in a Verto wallet (types like iban_to_wallet) rather than a payout.
func isCollection(e Event) bool { return strings.HasSuffix(e.Type, "_to_wallet") }
