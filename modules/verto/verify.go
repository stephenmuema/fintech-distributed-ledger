package verto

import (
	"crypto/rsa"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Verifier checks that a webhook really came from Verto. Verto does not sign the body: each
// delivery carries one or more auth-* headers (auth-0, auth-1: one per live webhook
// certificate), each an RSA-OAEP ciphertext (SHA-512 for both the OAEP digest and MGF1) of
// "base64(companyId)||<UTC day of month>", encrypted to the public half of a webhook
// certificate you download from the Verto platform. A delivery is genuine when one header
// decrypts with one of your private keys to your company id and a day within one of today.
//
// The day is a freshness hint, not a nonce: deduplicate on the event id before acting, as the
// core does for inward funds (provider reference) and outcomes (a closed item is a no-op).
type Verifier struct {
	CompanyID string
	Keys      []*rsa.PrivateKey
	Now       func() time.Time
}

var ErrUnverified = errors.New("verto webhook: no auth-* header decrypts to our company and a current day")

func (v *Verifier) Verify(h http.Header) error {
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	today := now().UTC()
	days := map[string]bool{}
	for _, d := range []time.Time{today.AddDate(0, 0, -1), today, today.AddDate(0, 0, 1)} {
		days[strconv.Itoa(d.Day())] = true
	}
	for name, values := range h {
		if !strings.HasPrefix(strings.ToLower(name), "auth-") {
			continue
		}
		for _, value := range values {
			ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
			if err != nil {
				continue
			}
			for _, key := range v.Keys {
				plain, err := rsa.DecryptOAEP(sha512.New(), nil, key, ciphertext, nil) // MGF1 uses the same hash
				if err != nil {
					continue
				}
				company, day, ok := strings.Cut(string(plain), "||")
				if !ok {
					continue
				}
				decoded, err := base64.StdEncoding.DecodeString(company)
				if err == nil && subtle.ConstantTimeCompare(decoded, []byte(v.CompanyID)) == 1 && days[day] {
					return nil
				}
			}
		}
	}
	return ErrUnverified
}

// ParsePrivateKey reads a webhook certificate's private key (RSA-4096, PKCS#8 PEM).
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("verto: no PEM block in private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("verto: private key is not RSA")
	}
	return rsaKey, nil
}
