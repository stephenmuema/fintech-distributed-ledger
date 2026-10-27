package chargebackclassifier

import (
	"context"
	"testing"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

func TestClassifiesByReasonCodeAndGuardsFraudClearings(t *testing.T) {
	c := New()
	if hooks := extapi.Implements(c); len(hooks) != 2 {
		t.Fatalf("hooks: %v", hooks)
	}
	out, _ := c.Classify(context.Background(), extapi.ClassifyRequest{Items: []extapi.SuspenseItem{{ID: "a", Reference: "CB-10.4-ARN7421"}, {ID: "b", Reference: "OTHER"}}})
	if len(out.Classifications) != 1 || out.Classifications[0].AssignTo != "ops.fraud" || out.Classifications[0].Attributes["reason_code"] != "10.4" {
		t.Fatalf("classifications: %+v", out)
	}
	veto, _ := c.ValidateClearing(context.Background(), extapi.ValidateClearingRequest{Item: extapi.SuspenseItem{Attributes: map[string]string{"reason_code": "10.4"}}})
	if veto.Allow || veto.Code != "case_number_required" {
		t.Fatalf("veto: %+v", veto)
	}
	ok, _ := c.ValidateClearing(context.Background(), extapi.ValidateClearingRequest{Item: extapi.SuspenseItem{Attributes: map[string]string{"reason_code": "10.4", "case_number": "F-1"}}})
	if !ok.Allow {
		t.Fatal("with a case number the clearing is allowed")
	}
}
