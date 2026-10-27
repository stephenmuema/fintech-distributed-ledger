// Command mpesa-module serves Zirnis's M-Pesa provider module (Safaricom Daraja).
//
// Configuration (environment):
//
//	ZIRNIS_MODULE_SECRET            shared secret; the module endpoint's secretEnv in Zirnis holds the same value
//	MODULE_ADDR                     listen address (default :8090)
//	ZIRNIS_PUBLIC_URL               Zirnis's public https URL; Daraja calls back to {it}/webhooks/mpesa/{token}/…
//	MPESA_CALLBACK_TOKEN            at least 32 random characters: the secret path segment of every callback URL
//	MPESA_ENV                       sandbox (default) or production
//	MPESA_BASE_URL                  override Daraja's base URL
//	MPESA_CONSUMER_KEY, MPESA_CONSUMER_SECRET   the Daraja app
//	MPESA_SHORTCODE, MPESA_PASSKEY  the paybill/till for STK push and C2B
//	MPESA_TRANSACTION_TYPE          CustomerPayBillOnline (default) or CustomerBuyGoodsOnline
//	MPESA_B2C_SHORTCODE             B2C shortcode (default: MPESA_SHORTCODE)
//	MPESA_INITIATOR_NAME, MPESA_SECURITY_CREDENTIAL   enable B2C payouts and transaction status queries
//	MPESA_REJECT_UNKNOWN_ACCOUNTS   true (default): refuse paybill payments to unknown account numbers
package main

import (
	"errors"
	"strings"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/mpesa"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

func main() {
	client, err := fromEnv()
	if err != nil {
		kit.Fatal(err)
	}
	if err := kit.Serve("mpesa", provapi.Module{mpesa.Name: client}); err != nil {
		kit.Fatal(err)
	}
}

func fromEnv() (*mpesa.Client, error) {
	var req kit.Required
	cfg := mpesa.Config{
		ConsumerKey: req.Get("MPESA_CONSUMER_KEY"), ConsumerSecret: req.Get("MPESA_CONSUMER_SECRET"),
		ShortCode: req.Get("MPESA_SHORTCODE"), PassKey: req.Get("MPESA_PASSKEY"),
		TransactionType: kit.Env("MPESA_TRANSACTION_TYPE", "CustomerPayBillOnline"), B2CShortCode: kit.Env("MPESA_B2C_SHORTCODE", ""),
		InitiatorName: kit.Env("MPESA_INITIATOR_NAME", ""), SecurityCredential: kit.Env("MPESA_SECURITY_CREDENTIAL", ""),
		CallbackToken: req.Get("MPESA_CALLBACK_TOKEN"), RejectUnknownAccounts: kit.Env("MPESA_REJECT_UNKNOWN_ACCOUNTS", "true") == "true",
	}
	publicURL := strings.TrimSuffix(req.Get("ZIRNIS_PUBLIC_URL"), "/")
	if err := req.Err(); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(publicURL, "https://") {
		return nil, errors.New("ZIRNIS_PUBLIC_URL must be an https URL: Daraja calls back over HTTPS")
	}
	if len(cfg.CallbackToken) < 32 {
		return nil, errors.New("MPESA_CALLBACK_TOKEN must be at least 32 random characters")
	}
	cfg.BaseURL = kit.Env("MPESA_BASE_URL", map[bool]string{true: mpesa.ProductionBaseURL, false: mpesa.SandboxBaseURL}[kit.Env("MPESA_ENV", "sandbox") == "production"])
	cfg.CallbackBaseURL = publicURL + "/webhooks/mpesa/" + cfg.CallbackToken
	return mpesa.New(cfg), nil
}
