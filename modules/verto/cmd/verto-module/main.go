// Command verto-module runs the Verto provider module as a remote extension, next to Zirnis:
//
//   - POST /hooks          Zirnis's suspense.opened deliveries (HMAC-signed with ZIRNIS_HOOK_SECRET)
//   - POST /verto/webhooks Verto's webhooks (auth-* headers, RSA-OAEP with your webhook certificate)
//
// Register it in Zirnis as hook endpoint "verto" (URL http://<host>:8090/hooks, hook
// suspense.opened, secretEnv ZIRNIS_HOOK_SECRET) and point a Verto webhook subscription at
// https://<public host>/verto/webhooks. Environment:
//
//	ZIRNIS_URL, ZIRNIS_HOOK_SECRET          the core and the shared secret
//	VERTO_COMPANY_ID, VERTO_WEBHOOK_KEY     your Verto company id; the webhook certificate's private key (PEM file)
//	VERTO_WALLETS                           walletId=accountNumber,… : which Zirnis account a Verto wallet's money is for
//	MODULE_ADDR                             listen address (default :8090)
//
// The Verto API client (payouts) is yours to wire: see PayoutsAPI and docs/guides/adding-a-provider.md.
package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/modules/verto"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	addr := os.Getenv("MODULE_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" { // container health probe
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://127.0.0.1" + addr[strings.LastIndex(addr, ":"):] + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	pemBytes, err := os.ReadFile(os.Getenv("VERTO_WEBHOOK_KEY"))
	if err != nil {
		log.Error("VERTO_WEBHOOK_KEY", "error", err)
		os.Exit(1)
	}
	key, err := verto.ParsePrivateKey(pemBytes)
	if err != nil {
		log.Error("VERTO_WEBHOOK_KEY", "error", err)
		os.Exit(1)
	}
	wallets := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("VERTO_WALLETS"), ",") {
		if wallet, account, ok := strings.Cut(strings.TrimSpace(pair), "="); ok {
			wallets[wallet] = account
		}
	}
	secret := os.Getenv("ZIRNIS_HOOK_SECRET")
	core := &extapi.Client{BaseURL: os.Getenv("ZIRNIS_URL"), Name: "verto", Secret: secret, HTTP: &http.Client{Timeout: 10 * time.Second}}
	verifier := &verto.Verifier{CompanyID: os.Getenv("VERTO_COMPANY_ID"), Keys: []*rsa.PrivateKey{key}}

	mux := http.NewServeMux()
	mux.Handle("/hooks", extapi.Handler(verto.New(notWired{}), secret))
	mux.Handle("/verto/webhooks", verto.Webhook(verifier, core, func(e verto.Event) string { return wallets[e.WalletID] },
		map[string]int32{"JPY": 0}, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","module":"verto"}`))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Info("verto module listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// notWired stands in until the Verto payments client is written: every payout waits in
// VERTOOUT (the core retries) rather than being guessed at.
type notWired struct{}

var errNotWired = errors.New("verto payouts client not wired yet")

func (notWired) FindByClientReference(context.Context, string) (*verto.Payout, error) {
	return nil, errNotWired
}
func (notWired) CreatePayout(context.Context, verto.PayoutRequest) (*verto.Payout, error) {
	return nil, errNotWired
}
