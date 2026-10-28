package httpapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/workers"
)

// These hold on every engine. On SQLite units of work run one at a time; run them on a server
// engine (ZIRNIS_TEST_DATASOURCE=postgres …) to exercise parallel writers.

func TestConcurrentPayoutsFromOneCustomersAccountsStayWithinTheDailyLimit(t *testing.T) {
	h := newHarness(t)
	customer := h.verifiedCustomer("Individual") // tier 1: USD 1,000 a payout, USD 3,000 a day
	main := h.account(customer, "USD")
	h.cardPayment(main, 600_000, "tok_visa")
	sub := h.platform().post("/accounts/"+main.ID+"/sub-accounts", map[string]any{"holderName": "Sarah Miller", "relationship": "spouse"}).expect(201)
	spouse := account{sub.str("id"), sub.str("accountNumber"), customer, "USD"}
	h.transfer(main, spouse, 250_000).expect(201) // between their own accounts: not outgoing

	var wg sync.WaitGroup
	statuses := make([]string, 8)
	for i := range statuses {
		from := main
		if i%2 == 1 {
			from = spouse
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = payout(h, from, 90_000, "Bank", "1234567890", "68").expect(201).str("status")
		}()
	}
	wg.Wait()

	accepted := 0
	for _, s := range statuses {
		if s != "Rejected" {
			accepted++
		}
	}
	check(t, accepted >= 1 && accepted <= 3, "USD 900 payouts accepted against a USD 3,000 day: ", accepted, statuses)
	h.assertLedgerBalanced()
}

func TestConcurrentOpeningsGiveACustomerOneAccountPerCurrency(t *testing.T) {
	h := newHarness(t)
	customer := h.verifiedCustomer("Individual")

	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = h.platform().post("/accounts", map[string]any{"customerId": customer, "currency": "EUR"}).status
		}()
	}
	wg.Wait()

	opened := 0
	for _, c := range codes {
		if c == 201 {
			opened++
		}
	}
	eq(t, 1, opened, codes)
}

func TestParallelCardPaymentsAllBookAndTheAuditTrailVerifies(t *testing.T) {
	h := newHarness(t)
	merchants := make([]account, 5)
	for i := range merchants {
		merchants[i] = h.account(h.verifiedCustomer("Business"))
	}

	var wg sync.WaitGroup
	statuses := make([]string, 4*len(merchants))
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = h.cardPayment(merchants[i%len(merchants)], 10_000, "tok_visa").str("status")
		}()
	}
	wg.Wait()

	for _, s := range statuses {
		eq(t, "Succeeded", s)
	}
	for _, m := range merchants {
		eq(t, 4*9_710, h.workingBalance(m.ID)) // each payment: 10,000 less the 2.9% fee
	}
	verified := h.amina().get("/ops/audit/verify").expect(200)
	check(t, verified.boolean("valid"), "the audit chains verify: ", verified)
	check(t, h.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
	h.assertLedgerBalanced()
}

func TestClosingAnAccountWhileMoneyArrivesNeverStrandsMoney(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(100_000, "Individual")
	target := h.account(h.verifiedCustomer("Individual"))

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.transfer(source, target, 1_000) // completes, or is refused once the account is closed
		}()
	}
	var closed int
	wg.Add(1)
	go func() {
		defer wg.Done()
		closed = h.platform().post("/accounts/"+target.ID+"/close", map[string]any{"reason": "Customer request"}).status
	}()
	wg.Wait()

	if closed == 200 {
		eq(t, 0, h.workingBalance(target.ID)) // closed only while empty, and nothing arrived after
	} else {
		eq(t, 422, closed) // money arrived first: not closable
	}
	h.assertLedgerBalanced()
}

func TestCloseOfBusinessDuringTrafficKeepsTheBooksWhole(t *testing.T) {
	h := newHarness(t)
	source := h.fundedAccount(200_000, "Individual")
	destination := h.account(h.verifiedCustomer("Individual"))

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.transfer(source, destination, 1_000)
		}()
	}
	cob := runCob(h)
	wg.Wait()

	eq(t, "Completed", cob.str("status"))
	check(t, h.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
	h.assertLedgerBalanced()
}

func TestBusyAccountsAreSplitAndEveryViewShowsTheirTotal(t *testing.T) {
	h := newHarness(t)
	merchants := make([]account, 4)
	for i := range merchants {
		merchants[i] = h.account(h.verifiedCustomer("Business"))
	}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eq(t, "Succeeded", h.cardPayment(merchants[i%len(merchants)], 10_000, "tok_visa").str("status"))
		}()
	}
	wg.Wait()

	if !h.sys.Store.Serial() {
		var parts int
		err := h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) (err error) {
			parts, err = store.Scalar[int](tx, "SELECT COUNT(*) FROM ledger_accounts WHERE parent_id IS NOT NULL")
			return err
		})
		check(t, err == nil && parts > 0, "parallel writers spread the busy accounts over parts: ", parts, err)
	}
	var float int64
	for _, p := range h.amina().get("/ops/treasury/positions").expect(200).items() {
		if p.str("currency") == "KES" {
			float += p.num("ledgerFloat")
		}
	}
	eq(t, int64(32*10_000), float) // every payment's gross, whichever part of a float it landed in
	fees := int64(0)
	for _, a := range h.amina().get("/ops/gl/internal-accounts").expect(200).items() {
		check(t, a.str("parentId") == "", "parts are not listed: ", a.str("accountNumber"))
		if a.str("code") == domain.FeeRevenue("KES").Code {
			fees = a.num("balance")
		}
	}
	eq(t, int64(32*290), fees)
	eq(t, 0, len(h.amina().get("/ops/suspense/integrity").expect(200).items()))
	check(t, h.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
	h.assertLedgerBalanced()
}

func TestASplitBookMustNotGoNegativeInTotal(t *testing.T) {
	h := newHarness(t)
	ledger := h.sys.App.Ledger
	book := domain.SystemAccountSpec{Code: domain.SuspenseAccountCode(domain.ChannelTypeCode("ACQUIRERA", domain.Outward), "KES")}
	post := func(reference string, in bool, amount int64) error {
		return h.sys.Store.Atomic(context.Background(), func(tx *store.Tx) error {
			b, err := ledger.SystemAccount(tx, book)
			if err != nil {
				return err
			}
			bank, err := ledger.SystemAccount(tx, domain.SettlementBank("KES"))
			if err != nil {
				return err
			}
			e := domain.NewEntry(reference, domain.CatSuspense, "Split book test")
			if in {
				e = e.Debit(bank, amount).Credit(b, amount)
			} else {
				e = e.Debit(b, amount).Credit(bank, amount)
			}
			_, err = ledger.Post(tx, e)
			return err
		})
	}
	for i := range 8 {
		if err := post(fmt.Sprintf("split-in-%d", i), true, 1_000); err != nil {
			t.Fatal(err)
		}
	}
	// Every withdrawal within the total passes, whichever part it lands in (a part may go below
	// zero while the book doesn't)...
	for i := range 8 {
		if err := post(fmt.Sprintf("split-out-%d", i), false, 1_000); err != nil {
			t.Fatal(err)
		}
	}
	// ...and one beyond it is refused.
	err := post("split-out-extra", false, 1)
	check(t, err != nil && strings.Contains(err.Error(), "insufficient funds"), "refused below zero in total: ", err)
	check(t, h.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
	h.assertLedgerBalanced()
}

func TestThePartitionedLedgerAndAuditTrailKeepEveryGuarantee(t *testing.T) {
	h := newHarness(t)
	if h.sys.Store.Dialect() != store.Postgres {
		t.Skip("monthly partitions are PostgreSQL only")
	}
	ctx := context.Background()
	merchant := h.account(h.verifiedCustomer("Business"))
	h.cardPayment(merchant, 10_000, "tok_visa")

	if err := h.sys.Store.Partition(ctx, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.sys.Store.Partition(ctx, h.clock.Now()); err != nil { // a second run changes nothing
		t.Fatal(err)
	}
	months, err := h.sys.Store.Partitions(ctx, "journal_lines")
	check(t, err == nil && len(months) >= 4, "this month, two ahead and the default: ", months, err)

	// Money keeps moving, and every check still holds.
	h.cardPayment(merchant, 10_000, "tok_visa")
	eq(t, 2*9_710, h.workingBalance(merchant.ID))
	verified := h.amina().get("/ops/audit/verify").expect(200)
	check(t, verified.boolean("valid"), "the audit chains verify across the move: ", verified)
	check(t, h.amina().get("/ops/gl/integrity").expect(200).boolean("healthy"), "balances agree with their postings")
	h.assertLedgerBalanced()
	for _, table := range []string{"journal_lines", "audit_events"} {
		err := h.sys.Store.Atomic(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec("DELETE FROM " + table)
			return err
		})
		check(t, err != nil && strings.Contains(err.Error(), "append-only"), table+" stays append-only: ", err)
	}

	// Upkeep opens the months ahead as time passes.
	h.clock.Advance(100 * 24 * time.Hour)
	if err := h.sys.Store.EnsurePartitions(ctx, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	audit, _ := h.sys.Store.Partitions(ctx, "audit_events")
	lines, _ := h.sys.Store.Partitions(ctx, "journal_lines")
	check(t, len(audit) > len(months) && len(lines) > len(months), "months opened ahead: ", lines, audit)
}

func TestSeveralDispatchersDeliverEachWebhookOnce(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	deliveries := map[string]int{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // a real endpoint takes a moment: the dispatchers overlap
		mu.Lock()
		deliveries[r.Header.Get("Zirnis-Event-Id")]++
		mu.Unlock()
	}))
	defer receiver.Close()
	merchant := h.account(h.verifiedCustomer("Business"))
	h.platform().send("PUT", "/customers/"+merchant.CustomerID+"/webhook-endpoint", map[string]any{"url": receiver.URL}, nil).expect(200)
	for range 12 {
		h.cardPayment(merchant, 10_000, "tok_visa")
	}

	// Four instances' dispatchers drain the same outbox at once.
	var wg sync.WaitGroup
	for range 4 {
		d := &workers.OutboxDispatcher{App: h.sys.App, Store: h.sys.Store, Client: receiver.Client(), Logger: slog.New(slog.DiscardHandler)}
		wg.Go(func() {
			if _, err := d.Drain(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	check(t, len(deliveries) >= 12, "every payment's event was delivered: ", len(deliveries))
	for id, n := range deliveries {
		eq(t, 1, n, "event "+id+" delivered once")
	}
}
