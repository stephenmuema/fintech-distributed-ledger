package httpapi_test

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/system"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// payerMatcher is the kind of extension a bank writes: it knows that unapplied M-Pesa receipts
// from a registered phone number belong to that customer's wallet.
type payerMatcher struct {
	wallets map[string]string // MSISDN → account number
	offered atomic.Int32
}

func (m *payerMatcher) Name() string { return "payer-matcher" }

func (m *payerMatcher) Classify(_ context.Context, req extapi.ClassifyRequest) (extapi.ClassifyResponse, error) {
	var out extapi.ClassifyResponse
	for _, item := range req.Items {
		out.Classifications = append(out.Classifications, extapi.Classification{ItemID: item.ID, AssignTo: "ops.amina"})
	}
	return out, nil
}

func (m *payerMatcher) Match(_ context.Context, req extapi.MatchRequest) (extapi.MatchResponse, error) {
	var out extapi.MatchResponse
	for _, item := range req.Items {
		m.offered.Add(1)
		if account, ok := m.wallets[item.Attributes["payer"]]; ok {
			out.Proposals = append(out.Proposals, extapi.ClearingProposal{ItemID: item.ID, TargetAccountNumber: account, Amount: item.OpenAmount,
				Narrative: "Payer's registered wallet", Confidence: 0.97})
		}
	}
	return out, nil
}

func unappliedDeposit(h *harness, reference, payer string, amount int64) {
	h.t.Helper()
	eq(h.t, "deposit_unapplied", h.platform().post("/sandbox/providers/mpesa/events", map[string]any{"type": "deposit.received", "reference": reference,
		"amount": amount, "currency": "KES", "accountReference": "TYPO", "payer": payer}).expect(200).str("outcome"))
}

func TestInProcessExtensionClassifiesAndAutoClearsAboveTheTypeThreshold(t *testing.T) {
	matcher := &payerMatcher{wallets: map[string]string{}}
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(matcher) })
	wallet := h.account(h.verifiedCustomer("Individual"))
	matcher.wallets["254700000001"] = wallet.AccountNumber

	bindExtension(h, "MPESA", "payer-matcher", 95)

	unappliedDeposit(h, "SBX-A", "254700000001", 45_000) // known payer: cleared straight through
	unappliedDeposit(h, "SBX-B", "254799999999", 12_000) // unknown payer: stays for a human

	run := h.amina().post("/ops/suspense/run-hooks", nil).expect(200)
	eq(t, 2, run.num("classified"))
	eq(t, 1, run.num("autoCleared"))
	eq(t, 45_000, h.workingBalance(wallet.ID))

	open := h.amina().get("/ops/suspense/items?type=MPESAIN").expect(200).items()
	eq(t, 1, len(open))
	eq(t, "ops.amina", open[0].str("assignedTo"))

	var cleared []J
	for _, item := range h.amina().get("/ops/suspense/items?type=MPESAIN&status=Cleared").expect(200).items() {
		if item.str("sourceType") == "deposit" {
			cleared = append(cleared, item)
		}
	}
	eq(t, 1, len(cleared))
	clearings := h.amina().get("/ops/suspense/items/" + cleared[0].str("id")).expect(200).items("clearings")
	eq(t, "hook:payer-matcher", clearings[0].str("inputter"))
	eq(t, "system.stp", clearings[0].str("authoriser"))
	eq(t, 97, clearings[0].num("confidence"))

	extensions := h.amina().get("/core/extensions").expect(200).items()
	eq(t, "payer-matcher", extensions[0].str("name"))
	eq(t, "InProcess", extensions[0].str("kind"))
	check(t, len(h.amina().get("/core/extensions/calls?extension=payer-matcher").expect(200).items()) >= 2, "calls are logged")
	h.assertLedgerBalanced()
}

func TestBelowThresholdProposalsWaitForAHumanChecker(t *testing.T) {
	matcher := &payerMatcher{wallets: map[string]string{}}
	h := newHarness(t, func(s *system.System) { s.App.Extensions.MustRegister(matcher) })
	wallet := h.account(h.verifiedCustomer("Individual"))
	matcher.wallets["254700000002"] = wallet.AccountNumber
	bindExtension(h, "MPESA", "payer-matcher", 0)
	unappliedDeposit(h, "SBX-C", "254700000002", 30_000)

	eq(t, "Completed", runCob(h).str("status")) // AC.SUSPENSE.MATCH proposes during COB
	pending := h.amina().get("/ops/suspense/clearings?status=Unauthorised").expect(200).items()
	eq(t, 1, len(pending))
	eq(t, 0, h.workingBalance(wallet.ID))
	h.amina().post("/ops/suspense/clearings/"+pending[0].str("id")+"/authorise", nil).expect(200) // the hook was the maker
	eq(t, 30_000, h.workingBalance(wallet.ID))

	runCob(h) // an item with a pending or finished clearing is never offered again
	eq(t, int32(1), matcher.offered.Load())
}

// limitValidator vetoes clearings above a limit — a remote "input routine".
type limitValidator struct{ limit int64 }

func (v limitValidator) Name() string { return "limit-check" }

func (v limitValidator) ValidateClearing(_ context.Context, req extapi.ValidateClearingRequest) (extapi.ValidateClearingResponse, error) {
	if req.Clearing.Amount > v.limit {
		return extapi.ValidateClearingResponse{Allow: false, Code: "limit_exceeded", Message: "Clearings above the desk limit need treasury approval"}, nil
	}
	return extapi.ValidateClearingResponse{Allow: true}, nil
}

func (v limitValidator) RunCob(_ context.Context, req extapi.CobJobRequest) (extapi.CobJobResponse, error) {
	return extapi.CobJobResponse{Records: 1, Summary: "checked " + req.BusinessDate}, nil
}

func TestRemoteExtensionVetoesClearingsAndFailsClosed(t *testing.T) {
	env := secretEnv(t, "ZIRNIS_TEST_HOOK_SECRET", "s3cret-shared-with-the-extension")
	remote := httptest.NewServer(extapi.Handler(limitValidator{limit: 5_000}, "s3cret-shared-with-the-extension"))
	defer remote.Close()
	h := newHarness(t)

	eq(t, "invalid_hook_endpoint", h.amina().post("/ops/parameters/hooks/limit-check", map[string]any{"url": remote.URL,
		"hooks": []string{"suspense.nonsense"}, "secretEnv": env}).expect(422).str("code"))
	h.amina().post("/ops/parameters/hooks/limit-check", map[string]any{"url": remote.URL, "hooks": []string{"suspense.validate_clearing", "cob.job"},
		"secretEnv": env, "failurePolicy": "FailClosed", "timeoutMs": 2000}).expect(202)
	authorisePending(h, "limit-check")

	suspenseType(h, "DESK", map[string]any{"name": "Desk suspense", "side": "Debit", "currencies": []string{"KES"},
		"allowedTargets": []string{"Internal"}, "hooks": []string{"limit-check"}})
	bank := internalAccount(h, domain.CategorySettlementBank)
	item := raiseItem(h, "DESK", 9_000, bank, nil)
	clear := func(amount int64) *response {
		return h.amina().send("POST", "/ops/suspense/items/"+item.str("id")+"/clearings", map[string]any{
			"targetAccountNumber": bank, "amount": amount, "narrative": "Traced"}, map[string]string{"Idempotency-Key": domain.NewID().String()})
	}
	vetoed := clear(9_000).expect(422)
	eq(t, "limit_exceeded", vetoed.str("code"))
	check(t, strings.Contains(vetoed.str("detail"), "limit-check"), "the veto names the extension")
	clear(5_000).expect(202)

	// The remote COB job runs inside Close of Business.
	run := runCob(h)
	eq(t, "Completed", run.str("status"))
	found := false
	for _, j := range run.items("jobs") {
		if j.str("name") == "EXT.LIMIT.CHECK" {
			found = true
			eq(t, "checked 2026-10-28", j.str("summary"))
		}
	}
	check(t, found, "EXT.LIMIT.CHECK ran")

	// A wrong secret is refused by the extension; FailClosed then blocks the input.
	if err := os.Setenv(env, "not-the-secret"); err != nil {
		t.Fatal(err)
	}
	other := raiseItem(h, "DESK", 1_000, bank, nil)
	eq(t, "extension_unavailable", h.amina().postIdempotent("/ops/suspense/items/"+other.str("id")+"/clearings", map[string]any{
		"targetAccountNumber": bank, "amount": 1_000, "narrative": "Traced"}).expect(422).str("code"))

	var info J
	for _, e := range h.amina().get("/core/extensions").expect(200).items() {
		if e.str("name") == "limit-check" {
			info = e
		}
	}
	eq(t, "Remote", info.str("kind"))
	eq(t, 1, info.num("consecutiveFailures"))
	check(t, strings.Contains(info.str("lastError"), "401"), "last error: ", info.str("lastError"))
}

func TestSuspenseTypeHooksMustExist(t *testing.T) {
	h := newHarness(t)
	eq(t, "unknown_extension", h.amina().post("/ops/suspense/types/GHOST", map[string]any{"name": "Ghost", "side": "Debit",
		"currencies": []string{"KES"}, "allowedTargets": []string{"Internal"}, "hooks": []string{"does-not-exist"}}).expect(422).str("code"))
}
