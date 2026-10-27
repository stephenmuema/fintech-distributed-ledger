package tietoloader

import (
	"context"
	"testing"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

func TestLoadsAndRejects(t *testing.T) {
	l := New(Simulator{})
	event := func(token string) extapi.FundsEvent {
		return extapi.FundsEvent{Channel: &extapi.Channel{Direction: "Outward"},
			Item: extapi.SuspenseItem{ID: "0193f1c2-aaaa", OpenAmount: 5000, Currency: "KES", Attributes: map[string]string{"card_token": token}}}
	}
	if out, _ := l.OnFunds(context.Background(), event("tok_1")); out.Action != extapi.ActionComplete {
		t.Fatalf("load: %+v", out)
	}
	if out, _ := l.OnFunds(context.Background(), event("blocked_1")); out.Action != extapi.ActionReturn {
		t.Fatalf("reject: %+v", out)
	}
	if out, _ := l.OnFunds(context.Background(), extapi.FundsEvent{Channel: &extapi.Channel{Direction: "Inward"}}); out.Action != "" {
		t.Fatalf("inward is not this module's business: %+v", out)
	}
}
