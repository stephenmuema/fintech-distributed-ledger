// The stablecoin provider protocol runs on MODULE_ADDR (default :8090), protected by
// ZIRNIS_MODULE_SECRET. No private key is accepted. See deploy/modules/stablecoin.env.example.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/modules/stablecoin"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

func main() {
	// Let the common server's healthcheck run without requiring production RPC configuration.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := kit.Serve("stablecoin", nil); err != nil {
			kit.Fatal(err)
		}
		return
	}
	var required kit.Required
	cfg := stablecoin.Config{RPCURL: required.Get("STABLECOIN_RPC_URL"), Token: required.Get("STABLECOIN_TOKEN"),
		PaymentsContract: required.Get("STABLECOIN_PAYMENTS_CONTRACT"), Treasury: required.Get("STABLECOIN_TREASURY")}
	chain := required.Get("STABLECOIN_CHAIN_ID")
	if err := required.Err(); err != nil {
		kit.Fatal(err)
	}
	var err error
	cfg.ChainID, err = strconv.ParseUint(chain, 10, 64)
	if err != nil {
		kit.Fatal(fmt.Errorf("STABLECOIN_CHAIN_ID must be a positive decimal integer"))
	}
	p, err := stablecoin.New(cfg, nil)
	if err != nil {
		kit.Fatal(err)
	}
	if err = kit.Serve("stablecoin", provapi.Module{kit.Env("STABLECOIN_PROVIDER", "usdc"): p}); err != nil {
		kit.Fatal(err)
	}
}
