package kcb

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// KCBBankCode is the bank's CBK code (the beneficiary bank code on a payout).
const KCBBankCode = "01"

// RTGSThreshold: interbank payouts at or above KES 1,000,000 settle over RTGS rather than
// PesaLink/EFT (in minor units).
const RTGSThreshold int64 = 1_000_000_00

// reference derives a deterministic, collision-resistant transfer reference of n characters from
// the payout's idempotency key: the same payout always carries the same bank reference, so a
// bank that de-duplicates on it can never pay twice.
func reference(idempotencyKey string, n int) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:n]
}

// insufficientFloat reports whether a bank's error means *our* account there is short — a
// treasury problem, so the payout fails over to another bank instead of failing.
func insufficientFloat(message string) bool {
	m := strings.ToLower(message)
	return strings.Contains(m, "insufficient") || strings.Contains(m, "not enough funds")
}

// unavailable classifies transport failures for a bank call. Money-moving calls handle
// kit.ErrAmbiguous themselves (as Pending) before they get here.
func unavailable(err error) error {
	var nd *kit.NotDeliveredError
	switch {
	case errors.As(err, &nd):
		return provapi.Unavailable("network_error")
	case errors.Is(err, kit.ErrAmbiguous):
		return provapi.Ambiguous("timeout")
	}
	return provapi.Unavailable("provider_error")
}
