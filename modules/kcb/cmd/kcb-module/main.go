// Command kcb-module serves Zirnis's KCB Buni provider module: the "kcb" bank rail and deposit
// notifications, and "kcb-mpesa" STK collections through KCB's M-Pesa gateway.
//
// Configuration (environment):
//
//	ZIRNIS_MODULE_SECRET        shared secret; the module endpoint's secretEnv in Zirnis holds the same value
//	MODULE_ADDR                 listen address (default :8090)
//	ZIRNIS_PUBLIC_URL           Zirnis's public https URL; KCB calls back to {it}/webhooks/kcb/…
//	KCB_CALLBACK_TOKEN          at least 32 random characters: the secret path segment of STK callback URLs
//	KCB_ENV                     uat (default) or production
//	KCB_BASE_URL, KCB_TOKEN_URL override KCB's URLs
//	KCB_CONSUMER_KEY, KCB_CONSUMER_SECRET   the Buni application
//	KCB_COMPANY_CODE            default KE0010001
//	KCB_DEBIT_ACCOUNT           our KCB settlement account (payouts are debited from it; IPN credits land in it)
//	KCB_STK_SHORTCODE, KCB_STK_PASSKEY      STK push through KCB (shared shortcode 522522 by default)
//	KCB_IPN_PUBLIC_KEY          KCB's public key (path or PEM) for IPN signatures; without it every IPN is refused
package main

import (
	"crypto/rsa"
	"errors"
	"log/slog"
	"strings"

	"github.com/stephenmuema/finnie/modules/kcb"
	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

func main() {
	k, err := fromEnv()
	if err != nil {
		kit.Fatal(err)
	}
	if err := kit.Serve("kcb", provapi.Module{kcb.KCBName: k, kcb.KCBMpesaName: kcb.NewKCBMpesa(k)}); err != nil {
		kit.Fatal(err)
	}
}

func fromEnv() (*kcb.KCB, error) {
	var req kit.Required
	cfg := kcb.KCBConfig{
		TokenURL: kit.Env("KCB_TOKEN_URL", ""), ConsumerKey: req.Get("KCB_CONSUMER_KEY"), ConsumerSecret: req.Get("KCB_CONSUMER_SECRET"),
		CompanyCode: kit.Env("KCB_COMPANY_CODE", ""), DebitAccount: req.Get("KCB_DEBIT_ACCOUNT"),
		STKShortCode: kit.Env("KCB_STK_SHORTCODE", ""), STKPassKey: kit.Env("KCB_STK_PASSKEY", ""), CallbackToken: req.Get("KCB_CALLBACK_TOKEN"),
	}
	publicURL := strings.TrimSuffix(req.Get("ZIRNIS_PUBLIC_URL"), "/")
	if err := req.Err(); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(publicURL, "https://") {
		return nil, errors.New("ZIRNIS_PUBLIC_URL must be an https URL: KCB calls back over HTTPS")
	}
	if len(cfg.CallbackToken) < 32 {
		return nil, errors.New("KCB_CALLBACK_TOKEN must be at least 32 random characters")
	}
	var ipnKey *rsa.PublicKey
	if path := kit.Env("KCB_IPN_PUBLIC_KEY", ""); path != "" {
		key, err := kit.LoadPublicKey(path)
		if err != nil {
			return nil, errors.New("KCB_IPN_PUBLIC_KEY: " + err.Error())
		}
		ipnKey = key
	} else {
		slog.Warn("KCB IPN public key not configured: deposit notifications from KCB will be refused")
	}
	cfg.IPNPublicKey = ipnKey
	cfg.BaseURL = kit.Env("KCB_BASE_URL", map[bool]string{true: kcb.KCBProductionURL, false: kcb.KCBUATBaseURL}[kit.Env("KCB_ENV", "uat") == "production"])
	cfg.CallbackBaseURL = publicURL + "/webhooks/kcb/" + cfg.CallbackToken
	return kcb.NewKCB(cfg), nil
}
