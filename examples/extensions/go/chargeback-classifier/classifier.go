// Package chargebackclassifier is an in-process Go extension: compiled into the Zirnis binary
// and consulted with no network hop. It classifies items in a card-chargeback suspense type by
// the reason code quoted in the item's reference, routes them to the right desk, and vetoes
// clearings that would close a fraud chargeback without a case number.
//
// Register it in cmd/zirnis/extensions.go:
//
//	sys.App.Extensions.MustRegister(chargebackclassifier.New())
//
// and add "chargeback-classifier" to the suspense type's extensions (with local fields
// reason_code and case_number).
package chargebackclassifier

import (
	"context"
	"strings"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

type Classifier struct {
	// Desks maps a reason-code family to the operator queue that works it.
	Desks map[string]string
}

func New() *Classifier {
	return &Classifier{Desks: map[string]string{"10": "ops.fraud", "12": "ops.disputes", "13": "ops.disputes"}}
}

func (c *Classifier) Name() string { return "chargeback-classifier" }

// reasonCode takes the network reason code from a reference like "CB-10.4-ARN7421".
func reasonCode(reference string) string {
	parts := strings.Split(reference, "-")
	if len(parts) >= 2 && strings.EqualFold(parts[0], "CB") {
		return parts[1]
	}
	return ""
}

func (c *Classifier) Classify(_ context.Context, req extapi.ClassifyRequest) (extapi.ClassifyResponse, error) {
	var out extapi.ClassifyResponse
	for _, item := range req.Items {
		code := reasonCode(item.Reference)
		if code == "" {
			continue
		}
		family, _, _ := strings.Cut(code, ".")
		out.Classifications = append(out.Classifications, extapi.Classification{
			ItemID: item.ID, Attributes: map[string]string{"reason_code": code}, AssignTo: c.Desks[family],
		})
	}
	return out, nil
}

func (c *Classifier) ValidateClearing(_ context.Context, req extapi.ValidateClearingRequest) (extapi.ValidateClearingResponse, error) {
	fraud := strings.HasPrefix(req.Item.Attributes["reason_code"], "10.")
	if fraud && req.Item.Attributes["case_number"] == "" {
		return extapi.ValidateClearingResponse{Allow: false, Code: "case_number_required",
			Message: "Fraud chargebacks are cleared only with a fraud case number on the item"}, nil
	}
	return extapi.ValidateClearingResponse{Allow: true}, nil
}
