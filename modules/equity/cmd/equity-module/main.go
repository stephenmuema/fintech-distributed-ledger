// Command equity-module serves Zirnis's Equity Bank provider module (Jenga API): the "equity"
// bank rail, balance enquiry and deposit notifications, and — when JENGA_CARDS=true — "jenga-pgw"
// card collections through Jenga's hosted checkout.
//
// Configuration (environment):
//
//	ZIRNIS_MODULE_SECRET        shared secret; the module endpoint's secretEnv in Zirnis holds the same value
//	MODULE_ADDR                 listen address (default :8090)
//	ZIRNIS_PUBLIC_URL           Zirnis's public https URL (payers return to {it}/checkout/jenga-pgw/return)
//	JENGA_ENV                   uat (default) or production
//	JENGA_BASE_URL              override Jenga's base URL
//	JENGA_API_KEY, JENGA_MERCHANT_CODE, JENGA_CONSUMER_SECRET   the Jenga merchant
//	JENGA_ACCOUNT_NUMBER, JENGA_ACCOUNT_NAME                    our Equity account
//	JENGA_PRIVATE_KEY           RSA private key (path or PEM) that signs requests
//	JENGA_IPN_USERNAME, JENGA_IPN_PASSWORD   Basic credentials Jenga sends with IPNs; without them every IPN is refused
//	JENGA_CARDS                 true to enable card checkout
//	JENGA_CHECKOUT_URL          override Jenga's checkout URL
package main

import (
	"errors"
	"strings"

	"github.com/stephenmuema/finnie/modules/equity"
	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

func main() {
	m, err := fromEnv()
	if err != nil {
		kit.Fatal(err)
	}
	if err := kit.Serve("equity", m); err != nil {
		kit.Fatal(err)
	}
}

func fromEnv() (provapi.Module, error) {
	var req kit.Required
	production := kit.Env("JENGA_ENV", "uat") == "production"
	cfg := equity.JengaConfig{
		BaseURL: kit.Env("JENGA_BASE_URL", map[bool]string{true: equity.JengaProductionURL, false: equity.JengaUATBaseURL}[production]),
		APIKey:  req.Get("JENGA_API_KEY"), MerchantCode: req.Get("JENGA_MERCHANT_CODE"), ConsumerSecret: req.Get("JENGA_CONSUMER_SECRET"),
		AccountNumber: req.Get("JENGA_ACCOUNT_NUMBER"), AccountName: kit.Env("JENGA_ACCOUNT_NAME", ""),
		IPNUsername: kit.Env("JENGA_IPN_USERNAME", ""), IPNPassword: kit.Env("JENGA_IPN_PASSWORD", ""),
	}
	keyPath := req.Get("JENGA_PRIVATE_KEY")
	cards := kit.Env("JENGA_CARDS", "false") == "true"
	publicURL := strings.TrimSuffix(kit.Env("ZIRNIS_PUBLIC_URL", ""), "/")
	if err := req.Err(); err != nil {
		return nil, err
	}
	key, err := kit.LoadPrivateKey(keyPath)
	if err != nil {
		return nil, errors.New("JENGA_PRIVATE_KEY: " + err.Error())
	}
	cfg.PrivateKey = key
	e := equity.NewEquity(cfg)
	m := provapi.Module{equity.EquityName: e}
	if cards {
		if !strings.HasPrefix(publicURL, "https://") {
			return nil, errors.New("ZIRNIS_PUBLIC_URL must be an https URL when card checkout is enabled (payers return to it from Jenga)")
		}
		checkoutURL := kit.Env("JENGA_CHECKOUT_URL", map[bool]string{true: equity.JengaCheckoutProductionURL, false: equity.JengaCheckoutUATURL}[production])
		m[equity.JengaPGWName] = equity.NewJengaPGW(e, checkoutURL, publicURL+"/checkout/"+equity.JengaPGWName+"/return")
	}
	return m, nil
}
