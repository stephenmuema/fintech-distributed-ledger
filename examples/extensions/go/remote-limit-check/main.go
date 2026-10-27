// Command remote-limit-check is a remote Go extension: a separate service the core calls over
// signed HTTP. It vetoes suspense clearings above a limit and contributes a COB job.
//
//	export ZIRNIS_HOOK_SECRET=$(openssl rand -hex 32)
//	go run ./examples/extensions/go/remote-limit-check -addr :8091 -limit 500000
//
// Register http://localhost:8091 as "limit-check" serving suspense.validate_clearing and
// cob.job, with secretEnv ZIRNIS_HOOK_SECRET, and authorise it as a second operator.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/stephenmuema/finnie/pkg/extapi"
)

type limitCheck struct{ limit int64 }

func (l limitCheck) Name() string { return "limit-check" }

func (l limitCheck) ValidateClearing(_ context.Context, req extapi.ValidateClearingRequest) (extapi.ValidateClearingResponse, error) {
	if req.Clearing.Amount > l.limit {
		return extapi.ValidateClearingResponse{Allow: false, Code: "limit_exceeded", Message: "Above the desk limit: treasury must approve"}, nil
	}
	return extapi.ValidateClearingResponse{Allow: true}, nil
}

func (l limitCheck) RunCob(_ context.Context, req extapi.CobJobRequest) (extapi.CobJobResponse, error) {
	// Real work goes here: e.g. export the day's suspense ageing to the risk data mart.
	return extapi.CobJobResponse{Records: 0, Summary: "limit-check ready for " + req.NextBusinessDate}, nil
}

func main() {
	addr := flag.String("addr", ":8091", "listen address")
	limit := flag.Int64("limit", 50_000_000, "largest clearing allowed, in minor units")
	flag.Parse()
	secret := os.Getenv("ZIRNIS_HOOK_SECRET")
	if secret == "" {
		log.Fatal("set ZIRNIS_HOOK_SECRET to the secret shared with the Zirnis core")
	}
	server := &http.Server{Addr: *addr, Handler: extapi.Handler(limitCheck{*limit}, secret), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("limit-check serving on %s", *addr)
	log.Fatal(server.ListenAndServe())
}
