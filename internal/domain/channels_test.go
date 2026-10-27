package domain

import (
	"testing"
	"time"
)

func TestEventBackoffThenFailure(t *testing.T) {
	e := &SuspenseEvent{Status: EventDelivering}
	var delays []time.Duration
	for e.Status != EventFailed {
		e.Attempts++
		e.Retry("down", testNow)
		if e.Status == EventPending {
			delays = append(delays, e.NextAttemptAt.Sub(testNow))
		}
	}
	if e.Attempts != MaxEventAttempts || len(delays) != MaxEventAttempts-1 {
		t.Fatalf("attempts %d, retries %d", e.Attempts, len(delays))
	}
	if delays[0] != 30*time.Second || delays[1] != time.Minute || delays[len(delays)-1] > time.Hour {
		t.Fatalf("delays: %v", delays)
	}
}

func TestChannelSpecRules(t *testing.T) {
	spec := ChannelSpec{Name: "Tieto", Outward: true, Currencies: []string{"kes"}, LocalFields: LocalFields{{Name: "card_token", Label: "Card", Type: FieldText, Required: true}}}
	if err := spec.Normalize(); err != nil || !*spec.STP || spec.Currencies[0] != "KES" {
		t.Fatalf("normalise: %v %+v", err, spec)
	}
	out := spec.TypeSpec(Outward)
	if out.Side != CreditSuspense || out.LocalFields[0].Name != ProviderFieldReference || len(out.LocalFields) != 2 {
		t.Fatalf("type spec: %+v", out)
	}
	for _, bad := range []ChannelSpec{
		{Name: "x", Currencies: []string{"KES"}},
		{Name: "x", Outward: true, Currencies: []string{"KES"}, LocalFields: LocalFields{{Name: ProviderFieldReference, Type: FieldText}}},
		{Name: "x", Outward: true, Currencies: []string{"KES"}, SettlementAccounts: map[string]string{"USD": "USD140030001"}},
		{Name: "x", Outward: true, Currencies: []string{"KES"}, MinAmount: 500, MaxAmount: 100},
	} {
		if err := bad.Normalize(); CodeOf(err) == "" {
			t.Fatalf("should be rejected: %+v", bad)
		}
	}
	ch := NewProviderChannel("TIETO", spec, testNow)
	spec.Outward, spec.Inward = false, true
	if err := ch.Apply(spec); CodeOf(err) != "channel_direction_fixed" {
		t.Fatalf("removing a direction: %v", err)
	}
	if ChannelTypeCode("AIRTEL", Inward) != "AIRTELIN" || ChannelTypeCode("AIRTEL", Outward) != "AIRTELOUT" {
		t.Fatal("type codes")
	}
}
