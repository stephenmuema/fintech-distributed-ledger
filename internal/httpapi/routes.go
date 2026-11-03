package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/webhook"
)

const (
	tagCore      = "EB · Core"
	tagCustomers = "ST · Customers"
	tagAccounts  = "AC · Accounts"
	tagFT        = "FT · Funds transfer"
	tagPayments  = "PP · Payments"
	tagLinks     = "PP · Payment links"
	tagPayouts   = "PP · Payouts"
	tagDisputes  = "PP · Disputes"
	tagWebhooks  = "PP · Provider webhooks"
	tagFX        = "FX · Foreign exchange"
	tagCards     = "CM · Cards"
	tagNetwork   = "CM · Card network"
	tagRisk      = "RK · Risk & limits"
	tagRecon     = "RC · Reconciliation"
	tagGL        = "RE · General ledger"
	tagSandbox   = "Sandbox"
)

var (
	limitParam = param{"limit", "integer", "Maximum rows to return", false}
	asOfParam  = param{"asOf", "string", "Closed business date (YYYY-MM-DD); omit for live balances", false}
)

// ---------------------------------------------------------------------------------------------
// EB — business dates, COB, parameters (maker-checker), adjustments, audit and operations

func (s *Server) mapCore() {
	s.add(route{method: "GET", path: "/core/modules", tag: tagCore, access: platformRole, summary: "Modules and the COB jobs each contributes",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			type job struct {
				Name  string          `json:"name"`
				Stage domain.CobStage `json:"stage"`
			}
			type module struct {
				*app.Module
				CobJobs []job `json:"cobJobs"`
			}
			out := []module{}
			for _, m := range s.app.Modules() {
				jobs := []job{}
				for _, j := range s.app.JobsOf(m) {
					jobs = append(jobs, job{j.Name(), j.Stage()})
				}
				out = append(out, module{m, jobs})
			}
			return ok(w, out)
		}})
	s.add(route{method: "GET", path: "/core/dates", tag: tagCore, access: platformRole, analystMay: true,
		summary: "Business dates (T24 DATES): today, last and next working day, online/COB status",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			dates, err := s.app.Dates.Get(s.store.Read(r.Context()))
			if err != nil {
				return err
			}
			return ok(w, dates)
		}})
	s.add(route{method: "GET", path: "/core/holidays", tag: tagCore, access: platformRole, summary: "Holiday calendar",
		handle: func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Ops.Holidays(r.Context())) }})

	// Close of Business
	s.add(route{method: "POST", path: "/ops/cob/run", tag: tagCore, access: operatorRole,
		summary: "Run (or resume a halted) Close of Business; rolls the business date on success",
		handle:  func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Cob.Run(r.Context())) }})
	s.add(route{method: "GET", path: "/ops/cob/runs", tag: tagCore, access: operatorRole, summary: "Recent COB runs",
		handle: func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Cob.List(r.Context())) }})
	s.add(route{method: "GET", path: "/ops/cob/runs/{id}", tag: tagCore, access: operatorRole, summary: "A COB run and its jobs",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Cob.Get(r.Context(), id))
		})})

	// Parameters (maker-checker)
	s.add(route{method: "GET", path: "/ops/parameters/charges", tag: tagCore, access: operatorRole, summary: "Charge types (T24 FT.COMMISSION.TYPE)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Parameters.ChargeTypes(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/parameters/interest", tag: tagCore, access: operatorRole, summary: "Credit interest conditions",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Parameters.InterestConditions(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/parameters/pending", tag: tagCore, access: operatorRole, summary: "Unauthorised (INAU) parameter changes",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Parameters.Pending(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/parameters/charges/{code}", tag: tagCore, access: operatorRole, status: http.StatusAccepted, body: chargeChangeRequest{},
		summary: "Input a charge change (INAU) — takes effect only after a second operator authorises",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req chargeChangeRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeCharge(r.Context(), chi.URLParam(r, "code"), app.ChargeTypeChange{Description: req.Description, BasisPoints: req.BasisPoints})
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "POST", path: "/ops/parameters/interest", tag: tagCore, access: operatorRole, status: http.StatusAccepted, body: interestChangeRequest{},
		summary: "Input an interest condition change (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req interestChangeRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeInterest(r.Context(), app.InterestConditionChange{Category: req.Category, Currency: req.Currency, AnnualRate: req.AnnualRate})
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "POST", path: "/ops/parameters/pending/{id}/authorise", tag: tagCore, access: operatorRole, summary: "Authorise a parameter change (second operator)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			if err := s.requireAdminFor(r, id); err != nil {
				return err
			}
			return respond(w)(s.app.Parameters.Authorise(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/parameters/pending/{id}/reject", tag: tagCore, access: operatorRole, body: reviewRequest{}, summary: "Reject a parameter change",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			if err := s.requireAdminFor(r, id); err != nil {
				return err
			}
			reason, err := reviewReason(r, "rejected")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Parameters.Reject(r.Context(), id, reason))
		})})

	// Manual adjustments (maker-checker)
	s.add(route{method: "GET", path: "/ops/adjustments", tag: tagCore, access: operatorRole, summary: "Manual adjustments",
		query: []param{{"status", "string", "Unauthorised | Authorised | Rejected", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.Unauthorised, domain.Authorised, domain.Rejected)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Adjustments.List(r.Context(), status))
		}})
	s.add(route{method: "POST", path: "/ops/adjustments", tag: tagCore, access: operatorRole, idempotent: true, status: http.StatusAccepted, body: adjustmentRequest{},
		summary: "Input a manual GL adjustment on internal / P&L accounts (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req adjustmentRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			for i, line := range req.Lines {
				direction, err := enumOr("lines", string(line.Direction), "", domain.Debit, domain.Credit)
				if err != nil || direction == "" {
					return badRequest("validation_failed", "Line %d needs a direction of Debit or Credit.", i+1)
				}
				req.Lines[i].Direction = direction
			}
			adjustment, err := s.app.Adjustments.Input(r.Context(), strings.ToUpper(req.Currency), req.Narrative, req.Lines)
			return accepted(w, "/ops/adjustments", adjustment, err)
		}})
	s.add(route{method: "POST", path: "/ops/adjustments/{id}/authorise", tag: tagCore, access: operatorRole, summary: "Authorise and post an adjustment (second operator)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Adjustments.Authorise(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/adjustments/{id}/reject", tag: tagCore, access: operatorRole, body: reviewRequest{}, summary: "Reject an adjustment",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			reason, err := reviewReason(r, "rejected")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Adjustments.Reject(r.Context(), id, reason))
		})})

	// Audit and record history
	s.add(route{method: "GET", path: "/ops/audit", tag: tagCore, access: operatorRole, summary: "Audit trail, newest first",
		query: []param{{"entityType", "string", "Filter by entity type", false}, {"entityId", "string", "Filter by entity id", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			q := r.URL.Query()
			return respond(w)(s.app.Ops.AuditEvents(r.Context(), optional(q.Get("entityType")), optional(q.Get("entityId")), limit))
		}})
	s.add(route{method: "GET", path: "/ops/audit/verify", tag: tagCore, access: operatorRole, summary: "Re-verify the tamper-evident audit hash chain",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Ops.VerifyAudit(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/analytics", tag: tagCore, access: analystRole,
		summary: "Business intelligence (analyst permission): KPIs against the previous period, volume and value by day (or hour), breakdowns by flow, channel, type, segment and provider, decline reasons, top merchants, customer and account mix",
		query: []param{{"from", "string", "First booking date (YYYY-MM-DD); default 29 days before to", false},
			{"to", "string", "Last booking date (YYYY-MM-DD); default today's business date", false},
			{"currency", "string", "Currency values are shown in; default KES", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			from, err := queryDate(r, "from")
			if err != nil {
				return err
			}
			to, err := queryDate(r, "to")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Analytics(r.Context(), app.AnalyticsQuery{From: from, To: to, Currency: strings.ToUpper(r.URL.Query().Get("currency"))}))
		}})
	s.add(route{method: "GET", path: "/ops/dashboard", tag: tagCore, access: operatorRole,
		summary: "The console's front page: customers, accounts, the last 24 hours' transactions by status, flow, value and hour, and what waits for a person",
		handle:  func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Dashboard(r.Context())) }})
	s.add(route{method: "GET", path: "/ops/changes", tag: tagCore, access: operatorRole,
		summary: "The change log: every field changed on a historised record, who changed it (an operator, or an API client and who it acted for), when, from what to what",
		query: []param{{"entityType", "string", "e.g. Customer, Wallet, Card, Transaction, Version", false}, {"entityId", "string", "Record id", false},
			{"changedBy", "string", "Who changed it, e.g. amina or platform/cust-8841", false}, {"from", "string", "From (RFC 3339)", false},
			{"to", "string", "Before (RFC 3339)", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			f := app.ChangeFilter{EntityType: q.Get("entityType"), ChangedBy: q.Get("changedBy")}
			var err error
			if f.EntityID, err = queryUUID(r, "entityId"); err != nil {
				return err
			}
			for name, target := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
				if v := q.Get(name); v != "" {
					t, err := time.Parse(time.RFC3339, v)
					if err != nil {
						return badRequest("bad_request", "Query parameter '%s' must be an RFC 3339 time.", name)
					}
					*target = &t
				}
			}
			if f.Limit, err = queryInt(r, "limit", 100); err != nil {
				return err
			}
			return respond(w)(s.app.Changes(r.Context(), f))
		}})
	s.add(route{method: "GET", path: "/ops/history/{entityType}/{id}", tag: tagCore, access: operatorRole,
		summary: "Previous versions of a record ($HIS), e.g. /ops/history/Customer/{id}",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Ops.History(r.Context(), chi.URLParam(r, "entityType"), id))
		})})

	// Outbox / webhooks
	s.add(route{method: "GET", path: "/ops/outbox", tag: tagCore, access: operatorRole, summary: "Outbound webhook events",
		query: []param{{"status", "string", "Pending | Delivered | DeadLettered | Skipped", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.OutboxPending, domain.OutboxDelivered, domain.OutboxDeadLettered, domain.OutboxSkipped)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Ops.Outbox(r.Context(), status))
		}})
	s.add(route{method: "POST", path: "/ops/outbox/{id}/replay", tag: tagCore, access: operatorRole, summary: "Re-deliver a webhook event",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Ops.ReplayOutbox(r.Context(), id))
		})})

	// Provider health / incident drills
	s.add(route{method: "GET", path: "/ops/providers", tag: tagCore, access: operatorRole, summary: "Provider circuit states",
		handle: func(w http.ResponseWriter, r *http.Request) error { return ok(w, s.app.Ops.Providers()) }})
	s.add(route{method: "POST", path: "/ops/providers/{name}/faults", tag: tagCore, access: operatorRole, body: faultsRequest{},
		summary: "Sandbox: make a simulated provider slow (latency, jitter), flaky (errorRate) or lose answers to calls it executed (responseLossRate); zeros clear them",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req faultsRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			f := app.ProviderFaults{Latency: time.Duration(req.LatencyMs) * time.Millisecond, Jitter: time.Duration(req.JitterMs) * time.Millisecond,
				ErrorRate: req.ErrorRate, ResponseLossRate: req.ResponseLossRate}
			if err := s.app.Ops.SetFaults(r.Context(), chi.URLParam(r, "name"), f); err != nil {
				return err
			}
			return ok(w, map[string]any{"name": chi.URLParam(r, "name"), "faults": req})
		}})
	s.add(route{method: "GET", path: "/ops/providers/{name}/simulated", tag: tagCore, access: operatorRole,
		summary: "Sandbox: how many operations succeeded at a simulated provider, whatever the core heard (compare after a drill)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			n, err := s.app.Ops.SimulatedSuccesses(chi.URLParam(r, "name"))
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"name": chi.URLParam(r, "name"), "succeeded": n})
		}})
	s.add(route{method: "POST", path: "/ops/providers/{name}/outage", tag: tagCore, access: operatorRole, body: outageRequest{},
		summary: "Sandbox: simulate a provider outage to exercise failover and circuit breakers",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req outageRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			name := chi.URLParam(r, "name")
			if err := s.app.Ops.SetOutage(r.Context(), name, req.Down); err != nil {
				return err
			}
			return ok(w, map[string]any{"name": name, "simulatedOutage": req.Down})
		}})
}

// ---------------------------------------------------------------------------------------------
// ST — customers (CIF), KYC/KYB, tiering and webhook configuration

func (s *Server) mapCustomers() {
	s.add(route{method: "POST", path: "/customers", tag: tagCustomers, access: platformRole, status: http.StatusCreated, body: createCustomerRequest{},
		summary: "Create a customer (CIF)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req createCustomerRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			typ, err := enumOr("type", req.Type, domain.Individual, domain.Individual, domain.Business, domain.FinancialInstitution)
			if err != nil {
				return err
			}
			c, err := s.app.Customers.Create(r.Context(), app.CreateCustomer{Type: typ, LegalName: req.LegalName, Email: req.Email,
				Phone: req.Phone, Country: req.Country, AccountOfficer: req.AccountOfficer})
			if err != nil {
				return err
			}
			return created(w, "/customers/"+c.ID.String(), customerFrom(c))
		}})
	s.add(route{method: "POST", path: "/onboarding", tag: tagCustomers, access: platformRole, idempotent: true, status: http.StatusCreated, body: onboardingRequest{},
		summary: "Sign a customer up: the customer, their KYC (mandatory when the onboarding policy says so) and their accounts, in one request",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req onboardingRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			if err := validateNested(&req, map[string]any{"customer": req.Customer, "kyc": req.Kyc}); err != nil {
				return err
			}
			typ, err := enumOr("customer.type", req.Customer.Type, domain.Individual, domain.Individual, domain.Business, domain.FinancialInstitution)
			if err != nil {
				return err
			}
			cmd := app.Onboard{Currencies: req.Currencies, Customer: app.CreateCustomer{Type: typ, LegalName: req.Customer.LegalName, Email: req.Customer.Email,
				Phone: req.Customer.Phone, Country: req.Customer.Country, AccountOfficer: req.Customer.AccountOfficer}}
			if k := req.Kyc; k != nil {
				cmd.Kyc = &app.KycSubmission{DocumentType: k.DocumentType, DocumentNumber: k.DocumentNumber, DateOfBirth: k.DateOfBirth,
					RegistrationNumber: k.RegistrationNumber, Directors: k.Directors}
			}
			customer, accounts, err := s.app.Customers.Onboard(r.Context(), cmd)
			if err != nil {
				return err
			}
			return created(w, "/customers/"+customer.ID.String(), map[string]any{"customer": customerFrom(customer), "accounts": accountsFrom(accounts)})
		}})
	s.add(route{method: "GET", path: "/onboarding/policy", tag: tagCustomers, access: platformRole,
		summary: "The onboarding policy in force: whether KYC is required to sign up and to open an account",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Customers.OnboardingPolicy(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/onboarding-policy", tag: tagCustomers, access: adminRole, status: http.StatusAccepted, body: app.OnboardingPolicyChange{},
		summary: "Make KYC mandatory or optional at sign-up (administrators; INAU: applies when a different operator authorises it)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var change app.OnboardingPolicyChange
			if err := decode(r, &change); err != nil {
				return err
			}
			request, err := s.app.Parameters.ProposeOnboardingPolicy(r.Context(), change)
			return accepted(w, "/ops/parameters/pending", request, err)
		}})
	s.add(route{method: "GET", path: "/customers", tag: tagCustomers, access: platformRole, summary: "Customers, newest first: search by CIF number or part of a name, email or mnemonic",
		query: []param{{"q", "string", "CIF number, or part of a name, email or mnemonic", false}, {"before", "integer", "Next page: customers with a lower CIF number", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 50)
			if err != nil {
				return err
			}
			before, err := queryInt(r, "before", 0)
			if err != nil {
				return err
			}
			customers, err := s.app.Customers.Search(r.Context(), r.URL.Query().Get("q"), int64(before), limit)
			if err != nil {
				return err
			}
			out := make([]customerResponse, len(customers))
			for i := range customers {
				out[i] = customerFrom(&customers[i])
			}
			return ok(w, out)
		}})
	s.add(route{method: "GET", path: "/customers/{id}", tag: tagCustomers, access: platformRole, summary: "A customer",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			c, err := s.app.Customers.Get(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, customerFrom(c))
		})})
	s.add(route{method: "GET", path: "/customers/by-number/{customerNumber}", tag: tagCustomers, access: platformRole,
		summary: "Customer position enquiry: customer and all accounts with balances",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			number, err := strconv.ParseInt(chi.URLParam(r, "customerNumber"), 10, 64)
			if err != nil {
				return notFound("customer", chi.URLParam(r, "customerNumber"))
			}
			c, err := s.app.Customers.GetByNumber(r.Context(), number)
			if err != nil {
				return err
			}
			accounts, err := s.app.Accounts.ListForCustomer(r.Context(), c.ID)
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"customer": customerFrom(c), "accounts": accountsFrom(accounts)})
		}})
	s.add(route{method: "POST", path: "/customers/{id}/details", tag: tagCustomers, access: platformRole, body: customerDetailsRequest{},
		summary: "Change a customer's details: email and phone apply at once (payouts in the next 24h are reviewed); a verified customer's legal name and country wait for a checker (202)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req customerDetailsRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			customer, held, err := s.app.Customers.UpdateDetails(r.Context(), id,
				domain.CustomerDetails{LegalName: req.LegalName, Email: req.Email, Phone: req.Phone, Country: req.Country}, req.Reason)
			if err != nil {
				return err
			}
			status := http.StatusOK
			if held != nil {
				status = http.StatusAccepted
			}
			return writeJSON(w, status, map[string]any{"customer": customerFrom(customer), "pendingChange": held})
		})})
	s.add(route{method: "GET", path: "/customers/{id}/changes", tag: tagCustomers, access: platformRole, query: []param{limitParam},
		summary: "Change log of a customer and their accounts, cards, standing orders and payment links: who changed which field, when, from what to what",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Customers.CustomerChanges(r.Context(), id, limit))
		})})
	s.add(route{method: "GET", path: "/customers/{id}/accounts", tag: tagCustomers, access: platformRole, summary: "A customer's accounts",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			accounts, err := s.app.Accounts.ListForCustomer(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, accountsFrom(accounts))
		})})
	s.add(route{method: "POST", path: "/customers/{id}/kyc", tag: tagCustomers, access: platformRole, body: kycRequest{},
		summary: "Run KYC (individuals) or KYB (businesses)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req kycRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			c, err := s.app.Customers.SubmitKyc(r.Context(), id, app.KycSubmission{DocumentType: req.DocumentType, DocumentNumber: req.DocumentNumber,
				DateOfBirth: req.DateOfBirth, RegistrationNumber: req.RegistrationNumber, Directors: req.Directors})
			if err != nil {
				return err
			}
			return ok(w, customerFrom(c))
		})})
	s.add(route{method: "GET", path: "/ops/kyc/pending", tag: tagCustomers, access: operatorRole, summary: "KYC submissions waiting for review",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Customers.PendingKyc(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/customers/{id}/kyc-decision", tag: tagCustomers, access: operatorRole, status: http.StatusAccepted, body: app.KycDecision{},
		summary: "Input a KYC decision on a pending submission (INAU: applied when a checker authorises it)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var d app.KycDecision
			if err := decode(r, &d); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeKycDecision(r.Context(), id, d)
			return accepted(w, "/ops/parameters/pending", change, err)
		})})
	s.add(route{method: "PUT", path: "/customers/{id}/webhook-endpoint", tag: tagCustomers, access: platformRole, body: webhookEndpointRequest{},
		summary: "Configure the webhook URL; returns the HMAC signing secret",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req webhookEndpointRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			endpoint, err := s.app.Customers.SetWebhookEndpoint(r.Context(), id, req.URL)
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"url": endpoint.URL, "signingSecret": endpoint.Secret})
		})})
	s.add(route{method: "POST", path: "/ops/customers/{id}/tier", tag: tagCustomers, access: operatorRole, status: http.StatusAccepted, body: setTierRequest{},
		summary: "Input a limits-tier override (INAU: applied when another operator authorises it)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req setTierRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeTier(r.Context(), id, req.Tier, req.Reason)
			return accepted(w, "/ops/parameters/pending", change, err)
		})})
}

// ---------------------------------------------------------------------------------------------
// AC — accounts: opening, balances, statements and posting restrictions

func (s *Server) mapAccounts() {
	s.add(route{method: "POST", path: "/accounts", tag: tagAccounts, access: platformRole, status: http.StatusCreated, body: openAccountRequest{},
		summary: "Open an account (wallet for individuals, current account for businesses)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req openAccountRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			wallet, err := s.app.Accounts.Open(r.Context(), req.CustomerID, req.Currency)
			if err != nil {
				return err
			}
			view, err := s.app.Accounts.Get(r.Context(), wallet.ID)
			if err != nil {
				return err
			}
			return created(w, "/accounts/"+wallet.ID.String(), accountFrom(*view))
		}})
	s.add(route{method: "POST", path: "/accounts/{id}/sub-accounts", tag: tagAccounts, access: platformRole, status: http.StatusCreated, body: subAccountRequest{},
		summary: "Open a sub-account under a main account, in its currency, for someone the customer names (spouse, child, …); the customer's KYC and limits cover it",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req subAccountRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			view, err := s.app.Accounts.OpenSubAccount(r.Context(), id, req.HolderName, req.Relationship)
			if err != nil {
				return err
			}
			return created(w, "/accounts/"+view.Wallet.ID.String(), accountFrom(*view))
		})})
	s.add(route{method: "POST", path: "/accounts/{id}/close", tag: tagAccounts, access: platformRole, body: closeAccountRequest{},
		summary: "Close an account at the customer's request; refused while it has a balance, unfinished transactions, cards, standing orders or payment links",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req closeAccountRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			view, err := s.app.Accounts.Close(r.Context(), id, req.Reason)
			if err != nil {
				return err
			}
			return ok(w, accountFrom(*view))
		})})
	s.add(route{method: "GET", path: "/accounts/{id}", tag: tagAccounts, access: platformRole, summary: "Account with working, locked and online actual balances",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			view, err := s.app.Accounts.Get(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, accountFrom(*view))
		})})
	s.add(route{method: "GET", path: "/accounts/by-number/{accountNumber}", tag: tagAccounts, access: platformRole, summary: "Account enquiry by account number",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			view, err := s.app.Accounts.GetByAccountNumber(r.Context(), chi.URLParam(r, "accountNumber"))
			if err != nil {
				return err
			}
			return ok(w, accountFrom(*view))
		}})
	s.add(route{method: "GET", path: "/accounts/{id}/statement", tag: tagAccounts, access: platformRole,
		summary: "Statement entries (STMT.ENTRY) with running balance, booking and value dates",
		query:   []param{{"from", "string", "From booking date (YYYY-MM-DD)", false}, {"to", "string", "To booking date (YYYY-MM-DD)", false}, limitParam},
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			from, err := queryDate(r, "from")
			if err != nil {
				return err
			}
			to, err := queryDate(r, "to")
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Accounts.Statement(r.Context(), id, from, to, limit))
		})})
	s.add(route{method: "POST", path: "/ops/accounts/{id}/restriction", tag: tagAccounts, access: operatorRole, status: http.StatusAccepted, body: restrictionRequest{},
		summary: "Input a posting restriction (debit / credit / full block, or None to lift it); INAU until authorised",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req restrictionRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			restriction, err := enumOr("restriction", req.Restriction, domain.RestrictNone, domain.RestrictNone, domain.DebitBlock, domain.CreditBlock, domain.FullBlock)
			if err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeRestriction(r.Context(), id, restriction, req.Reason)
			return accepted(w, "/ops/parameters/pending", change, err)
		})})
}

// ---------------------------------------------------------------------------------------------
// FT — book transfers and account-number funds transfers

func (s *Server) mapTransfers() {

	// Standing orders (T24 STANDING.ORDER), paid by Close of Business
	s.add(route{method: "POST", path: "/standing-orders", tag: tagFT, access: platformRole, idempotent: true, status: http.StatusCreated, body: createStandingOrderRequest{},
		summary: "Schedule a recurring transfer or payout; COB pays each due date",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req createStandingOrderRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			cmd := app.CreateStandingOrder{WalletID: req.AccountID, Amount: req.Amount, StartDate: req.StartDate, EndDate: req.EndDate,
				Narration: req.Narration, DestinationWalletID: req.DestinationAccountID}
			frequency, err := enumOr("frequency", req.Frequency, domain.Monthly, domain.Daily, domain.Weekly, domain.Monthly)
			if err != nil {
				return err
			}
			cmd.Frequency = frequency
			if p := req.Payout; p != nil {
				if err := validate(p); err != nil {
					return err
				}
				rail, err := enumOr("rail", p.Rail, domain.RailMobileMoney, domain.RailMobileMoney, domain.RailBank)
				if err != nil {
					return err
				}
				destination, err := domain.NewPayoutDestination(rail, p.DestinationAccount, p.DestinationName, p.BankCode)
				if err != nil {
					return err
				}
				cmd.Destination = &destination
			}
			order, err := s.app.StandingOrders.Create(r.Context(), cmd)
			if err != nil {
				return err
			}
			return created(w, "/standing-orders/"+order.ID.String(), order)
		}})
	s.add(route{method: "GET", path: "/standing-orders", tag: tagFT, access: platformRole, summary: "An account's standing orders",
		query: []param{{"accountId", "string", "The debit account", true}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			accountID, err := uuid.Parse(r.URL.Query().Get("accountId"))
			if err != nil {
				return badRequest("validation_failed", "Query parameter 'accountId' must be an account id.")
			}
			return respond(w)(s.app.StandingOrders.List(r.Context(), accountID))
		}})
	s.add(route{method: "GET", path: "/standing-orders/{id}", tag: tagFT, access: platformRole, summary: "A standing order and its payments",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.StandingOrders.Get(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/standing-orders/{id}/cancel", tag: tagFT, access: platformRole, summary: "Cancel a standing order",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.StandingOrders.Cancel(r.Context(), id))
		})})
}

// ---------------------------------------------------------------------------------------------
// PP — collections, refunds, payment links, payouts, disputes and provider webhooks

// paymentLinkView is a payment link with the address payers open: the edge's public URL, when
// ZIRNIS_PUBLIC_URL is set.
type paymentLinkView struct {
	domain.PaymentLink
	URL string `json:"url,omitempty"`
}

func (s *Server) linkView(l domain.PaymentLink) paymentLinkView {
	v := paymentLinkView{PaymentLink: l}
	if s.cfg.PublicURL != "" {
		v.URL = strings.TrimSuffix(s.cfg.PublicURL, "/") + "/checkout/" + url.PathEscape(l.Code)
	}
	return v
}

func (s *Server) mapPayments() {

	s.add(route{method: "POST", path: "/payment-links", tag: tagLinks, access: platformRole, status: http.StatusCreated, body: createPaymentLinkRequest{},
		summary: "Create a shareable hosted payment link",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req createPaymentLinkRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			link, err := s.app.Links.Create(r.Context(), app.CreatePaymentLink{WalletID: req.AccountID, Amount: req.Amount,
				Description: req.Description, ExpiresAt: req.ExpiresAt, MaxUses: req.MaxUses})
			if err != nil {
				return err
			}
			return created(w, "/pay/"+link.Code, s.linkView(*link))
		}})
	s.add(route{method: "GET", path: "/payment-links", tag: tagLinks, access: platformRole, summary: "Payment links of an account",
		query: []param{{"accountId", "string", "Account id", true}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			accountID, err := queryUUID(r, "accountId")
			if err != nil {
				return err
			}
			if accountID == nil {
				return badRequest("validation_failed", "Query parameter 'accountId' is required.")
			}
			links, err := s.app.Links.List(r.Context(), *accountID)
			if err != nil {
				return err
			}
			views := make([]paymentLinkView, len(links))
			for i, l := range links {
				views[i] = s.linkView(l)
			}
			return ok(w, views)
		}})
	s.add(route{method: "POST", path: "/payment-links/{id}/deactivate", tag: tagLinks, access: platformRole, summary: "Deactivate a payment link",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			link, err := s.app.Links.Deactivate(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, s.linkView(*link))
		})})

	// Public hosted-checkout API (no API key; the link code is the capability), also on the edge.
	s.add(route{method: "GET", path: "/pay/{code}", tag: tagLinks, access: public, publicLimit: true, edge: true, summary: "Hosted checkout: link details",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			link, err := s.app.Links.GetByCode(r.Context(), chi.URLParam(r, "code"))
			if err != nil {
				return err
			}
			// How the payer can pay: a push to their phone, a card on a provider's own page, or (only
			// in the sandbox, where nobody has a real card) a test card token typed in.
			sandbox := s.cfg.SandboxEnabled()
			methods := map[string]bool{
				"mobileMoney": len(s.app.Router.CollectionRoute(domain.MethodMobileMoney, link.Currency)) > 0,
				"hostedCard":  len(s.app.Router.HostedCollectionRoute(domain.MethodCard, link.Currency)) > 0,
				"cardToken":   sandbox && len(s.app.Router.CollectionRoute(domain.MethodCard, link.Currency)) > 0,
				"stablecoin":  len(s.app.Router.CollectionRoute(domain.MethodStablecoin, link.Currency)) > 0,
			}
			return ok(w, map[string]any{"code": link.Code, "description": link.Description, "amount": link.Amount,
				"currency": link.Currency, "isActive": link.IsActive, "expiresAt": link.ExpiresAt, "methods": methods, "sandbox": sandbox})
		}})
	s.add(route{method: "POST", path: "/pay/{code}", tag: tagLinks, access: public, publicLimit: true, edge: true, idempotent: true, status: http.StatusCreated,
		body: payLinkRequest{}, summary: "Hosted checkout: pay a link",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req payLinkRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			method, err := enumOr("method", req.Method, domain.MethodCard, domain.MethodCard, domain.MethodMobileMoney, domain.MethodStablecoin)
			if err != nil {
				return err
			}
			link, err := s.app.Links.GetByCode(r.Context(), chi.URLParam(r, "code"))
			if err != nil {
				return err
			}
			fields := map[string]string{"linkCode": link.Code, "method": string(method), "source": req.Source}
			if req.Amount != nil {
				fields["amount"] = strconv.FormatInt(*req.Amount, 10)
			}
			if c := req.Customer; c != nil {
				fields["payer.firstName"], fields["payer.lastName"], fields["payer.email"] = c.FirstName, c.LastName, c.Email
				fields["payer.phone"], fields["payer.address"], fields["payer.postalCode"] = c.Phone, c.Address, c.PostalCode
			}
			view, err := s.app.Transactions.Input(r.Context(), app.InputTransaction{Version: derefOr(link.VersionName, "COLLECTION.LINK"),
				Channel: domain.ChannelLink, Fields: fields})
			if err != nil {
				return err
			}
			// The payer sees the payment's outcome, never the merchant's records.
			response := map[string]any{"reference": view.Reference, "paymentId": view.FlowRecordID, "status": view.Status, "amount": view.Amount, "currency": view.Currency,
				"failureCode": view.DeclineCode, "providerReference": view.ProviderReference}
			if view.NextAction != nil {
				response["nextAction"] = view.NextAction
			}
			return created(w, "/pay/"+link.Code, response)
		}})

	s.add(route{method: "GET", path: "/pay/{code}/payments/{id}", tag: tagLinks, access: public, publicLimit: true, edge: true,
		summary: "Hosted checkout: this link's payment status",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			payment, err := s.app.Links.PaymentStatus(r.Context(), chi.URLParam(r, "code"), id)
			if err != nil {
				return err
			}
			status := "Processing"
			if payment.Status == domain.PaymentSucceeded {
				status = "Completed"
			}
			if payment.Status == domain.PaymentFailed {
				status = "Failed"
			}
			return ok(w, map[string]any{"paymentId": payment.ID, "status": status, "amount": payment.Amount, "currency": payment.Currency,
				"failureCode": payment.FailureCode, "providerReference": payment.ProviderReference})
		})})

	s.mapPayouts()
	s.mapDisputes()

	// The sandbox simulator's callbacks, in Zirnis's own event format: HMAC-signed over the raw
	// body with a 5-minute replay window, then de-duplicated by event id. Real providers' callbacks
	// go through their module (/webhooks/{provider}/…).
	s.add(route{method: "POST", path: "/webhooks/providers/{provider}", tag: tagWebhooks, access: public, dedupedBy: "provider event id",
		summary: "Simulated provider callback (charge.succeeded|failed, payout.succeeded|failed, deposit.received, dispute.opened)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				return badRequest("bad_request", "Could not read the request body.")
			}
			provider := chi.URLParam(r, "provider")
			secret, known := s.cfg.WebhookSecrets[provider]
			// Only the simulator posts here: a secret for anything else signs nothing.
			p := s.app.Providers.Get(provider)
			known = known && p != nil && !p.Live()
			if !known || !webhook.Verify(r.Header.Get("Zirnis-Signature"), secret, string(body), s.app.Clock.Now()) {
				writeProblem(w, newProblem(r, http.StatusUnauthorized, "invalid_signature", "Invalid webhook signature."))
				return nil
			}
			var evt app.ProviderEvent
			if json.Unmarshal(body, &evt) != nil || evt.EventID == "" {
				return badRequest("malformed_event", "Malformed event.")
			}
			outcome, err := s.app.Validations.Validate(r.Context(), provider, evt, string(body))
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"received": true, "outcome": outcome})
		}})
}

func (s *Server) mapPayouts() {
	s.add(route{method: "GET", path: "/ops/payouts", tag: tagPayouts, access: operatorRole,
		summary: "Payout queue (e.g. ?status=PendingReview), or ?escalated=true: payouts no provider confirmed, waiting for operations to settle them",
		query: []param{{"status", "string", "PendingReview | Queued | Processing | Paid | Failed | Rejected", false},
			{"escalated", "boolean", "Only payouts escalated to operations and not yet settled", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.PayoutPendingReview, domain.PayoutQueued, domain.PayoutProcessing, domain.PayoutPaid, domain.PayoutFailed, domain.PayoutRejected)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			payouts, err := s.app.Payouts.List(r.Context(), status, r.URL.Query().Get("escalated") == "true", limit)
			if err != nil {
				return err
			}
			out := make([]payoutResponse, len(payouts))
			for i := range payouts {
				out[i] = payoutFrom(&payouts[i])
			}
			return ok(w, out)
		}})
	s.add(route{method: "POST", path: "/ops/payouts/{id}/approve", tag: tagPayouts, access: operatorRole, summary: "Release a payout held for review",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			payout, err := s.app.Payouts.Approve(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, payoutFrom(payout))
		})})
	s.add(route{method: "POST", path: "/ops/payouts/{id}/reject", tag: tagPayouts, access: operatorRole, body: reviewRequest{}, summary: "Reject a payout held for review",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			reason, err := reviewReason(r, "rejected_by_review")
			if err != nil {
				return err
			}
			payout, err := s.app.Payouts.Reject(r.Context(), id, reason)
			if err != nil {
				return err
			}
			return ok(w, payoutFrom(payout))
		})})
}

func (s *Server) mapDisputes() {
	s.add(route{method: "GET", path: "/disputes", tag: tagDisputes, access: platformRole, summary: "Disputes by evidence deadline",
		query: []param{{"status", "string", "Open | EvidenceSubmitted | Won | Lost", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.DisputeOpen, domain.DisputeEvidenceSubmitted, domain.DisputeWon, domain.DisputeLost)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Disputes.List(r.Context(), status))
		}})
	s.add(route{method: "GET", path: "/disputes/{id}", tag: tagDisputes, access: platformRole, summary: "A dispute",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Disputes.Get(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/disputes/{id}/evidence", tag: tagDisputes, access: platformRole, body: evidenceRequest{}, summary: "Submit evidence",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req evidenceRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Disputes.SubmitEvidence(r.Context(), id, req.Evidence))
		})})
	s.add(route{method: "POST", path: "/ops/disputes", tag: tagDisputes, access: operatorRole, idempotent: true, status: http.StatusAccepted, body: openDisputeRequest{},
		summary: "Input a chargeback against a payment (INAU: the merchant's funds are held when another operator authorises it)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req openDisputeRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeDispute(r.Context(), req.PaymentID, req.Amount, req.Reason)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "POST", path: "/ops/disputes/{id}/resolve", tag: tagDisputes, access: operatorRole, status: http.StatusAccepted, body: resolveDisputeRequest{},
		summary: "Input a dispute's outcome (won releases the hold; lost returns funds to the payer); INAU until authorised",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req resolveDisputeRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeDisputeResolution(r.Context(), id, req.MerchantWon)
			return accepted(w, "/ops/parameters/pending", change, err)
		})})
}

// ---------------------------------------------------------------------------------------------
// FX — firm quotes and conversions

func (s *Server) mapFx() {
	s.add(route{method: "GET", path: "/ops/fx/rates", tag: tagFX, access: operatorRole, summary: "Treasury's mid rates per US dollar, with their age",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.CurrencyRates(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/fx/rates", tag: tagFX, access: operatorRole, status: http.StatusAccepted, body: app.RatesChange{},
		summary: "Input mid rates per US dollar (INAU: they apply when a checker authorises them)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var change app.RatesChange
			if err := decode(r, &change); err != nil {
				return err
			}
			request, err := s.app.Parameters.ProposeRates(r.Context(), change)
			return accepted(w, "/ops/parameters/pending", request, err)
		}})
	s.add(route{method: "GET", path: "/fx/rates", tag: tagFX, access: platformRole, summary: "Mid-market rate",
		query: []param{{"from", "string", "Source currency", true}, {"to", "string", "Target currency", true}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			from, to := strings.ToUpper(r.URL.Query().Get("from")), strings.ToUpper(r.URL.Query().Get("to"))
			if from == "" || to == "" {
				return badRequest("validation_failed", "Query parameters 'from' and 'to' are required.")
			}
			rate, err := s.app.Rates.MidRate(from, to)
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"from": from, "to": to, "mid": rate, "asOf": s.app.Rates.AsOf()})
		}})
	s.add(route{method: "POST", path: "/fx/quotes", tag: tagFX, access: platformRole, status: http.StatusCreated, body: fxQuoteRequest{},
		summary: "Firm quote valid for a short window; fee shown explicitly",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req fxQuoteRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			quote, err := s.app.Fx.Quote(r.Context(), req.CustomerID, req.From, req.To, req.Amount)
			if err != nil {
				return err
			}
			return created(w, "/fx/quotes", quote)
		}})
	s.add(route{method: "POST", path: "/fx/conversions", tag: tagFX, access: platformRole, idempotent: true, body: fxConversionRequest{},
		summary: "Execute a quote between two of the customer's accounts",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req fxConversionRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Fx.Execute(r.Context(), req.QuoteID, req.SourceAccountID, req.TargetAccountID))
		}})
}

// ---------------------------------------------------------------------------------------------
// CM — card issuing, plus the card-network-facing authorisation API

func (s *Server) mapCards() {
	s.add(route{method: "POST", path: "/cards", tag: tagCards, access: platformRole, idempotent: true, status: http.StatusCreated, body: issueCardRequest{},
		summary: "Issue a virtual card on an account",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req issueCardRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			card, err := s.app.Cards.Issue(r.Context(), req.AccountID, req.PerTransactionLimit)
			if err != nil {
				return err
			}
			return created(w, "/cards/"+card.ID.String(), cardFrom(card, true))
		}})
	s.add(route{method: "GET", path: "/cards", tag: tagCards, access: platformRole, summary: "A customer's cards, or an account's",
		query: []param{{"customerId", "string", "Customer id", false}, {"accountId", "string", "Account id", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			customerID, err := queryUUID(r, "customerId")
			if err != nil {
				return err
			}
			accountID, err := queryUUID(r, "accountId")
			if err != nil {
				return err
			}
			cards, err := s.app.Cards.List(r.Context(), customerID, accountID)
			if err != nil {
				return err
			}
			out := make([]cardResponse, len(cards))
			for i := range cards {
				out[i] = cardFrom(&cards[i], false)
			}
			return ok(w, out)
		}})
	s.add(route{method: "GET", path: "/cards/{id}", tag: tagCards, access: platformRole, summary: "A card (the token is never returned on reads)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			card, err := s.app.Cards.Get(r.Context(), id)
			if err != nil {
				return err
			}
			return ok(w, cardFrom(card, false))
		})})
	for action, status := range map[string]domain.CardStatus{"freeze": domain.CardFrozen, "unfreeze": domain.CardActive, "terminate": domain.CardTerminated} {
		s.add(route{method: "POST", path: "/cards/{id}/" + action, tag: tagCards, access: platformRole, summary: strings.ToUpper(action[:1]) + action[1:] + " a card",
			handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
				card, err := s.app.Cards.ChangeStatus(r.Context(), id, status)
				if err != nil {
					return err
				}
				return ok(w, cardFrom(card, false))
			})})
	}
	s.add(route{method: "GET", path: "/cards/{id}/authorizations", tag: tagCards, access: platformRole, summary: "Recent authorisations on a card",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Cards.Authorizations(r.Context(), id))
		})})

	// Issuer processor / card network interface, authenticated by HMAC signature.
	s.add(route{method: "POST", path: "/card-network/authorizations", tag: tagNetwork, access: cardNetwork, body: networkAuthorizationRequest{},
		summary: "Real-time authorisation decision; idempotent on networkReference",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req networkAuthorizationRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			auth, err := s.app.Cards.Authorize(r.Context(), req.command())
			if err != nil {
				return err
			}
			return ok(w, networkAuthorizationFrom(auth))
		}})
	s.add(route{method: "POST", path: "/card-network/authorizations/{id}/capture", tag: tagNetwork, access: cardNetwork, body: captureRequest{},
		summary: "Clear (capture) an authorisation, fully or partially",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req captureRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Cards.Capture(r.Context(), id, req.Amount))
		})})
	s.add(route{method: "POST", path: "/card-network/authorizations/{id}/reverse", tag: tagNetwork, access: cardNetwork, summary: "Reverse an authorisation",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Cards.Reverse(r.Context(), id))
		})})
}

// ---------------------------------------------------------------------------------------------
// RK — blocklist and risk assessment history

func (s *Server) mapRisk() {
	s.add(route{method: "GET", path: "/ops/risk/blocklist", tag: tagRisk, access: operatorRole, summary: "Blocklisted counterparties",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Ops.Blocklist(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/risk/blocklist", tag: tagRisk, access: operatorRole, status: http.StatusAccepted, body: blocklistRequest{},
		summary: "Input a blocklist entry for a counterparty (account or MSISDN); INAU until authorised",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req blocklistRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeBlock(r.Context(), req.Value, req.Reason)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/risk/assessments", tag: tagRisk, access: operatorRole, summary: "Risk assessments",
		query: []param{{"customerId", "string", "Filter by customer", false}, {"decision", "string", "Approve | Review | Decline", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			customerID, err := queryUUID(r, "customerId")
			if err != nil {
				return err
			}
			decision, err := queryEnum(r, "decision", domain.RiskApprove, domain.RiskReview, domain.RiskDecline)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Ops.Assessments(r.Context(), customerID, decision))
		}})
}

// ---------------------------------------------------------------------------------------------
// RC — provider reconciliation and settlement

func (s *Server) mapReconciliation() {
	s.add(route{method: "POST", path: "/ops/reconciliation/{provider}", tag: tagRecon, access: operatorRole, status: http.StatusCreated,
		summary: "Upload a provider settlement report (CSV: provider_reference,type,amount,fee,currency)",
		query:   []param{{"date", "string", "Report date (YYYY-MM-DD)", true}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			date, err := queryDate(r, "date")
			if err != nil {
				return err
			}
			if date == nil {
				return badRequest("validation_failed", "Query parameter 'date' is required.")
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
			if err != nil {
				return badRequest("bad_request", "Could not read the request body.")
			}
			report, err := app.ParseSettlementCSV(string(body))
			if err != nil {
				return err
			}
			run, err := s.app.Recon.Run(r.Context(), chi.URLParam(r, "provider"), *date, report)
			if err != nil {
				return err
			}
			return created(w, "/ops/reconciliation/runs/"+run.ID.String(), run)
		}})
	s.add(route{method: "GET", path: "/ops/reconciliation/runs", tag: tagRecon, access: operatorRole, summary: "Reconciliation runs",
		handle: func(w http.ResponseWriter, r *http.Request) error { return respond(w)(s.app.Recon.List(r.Context())) }})
	s.add(route{method: "GET", path: "/ops/reconciliation/runs/{id}", tag: tagRecon, access: operatorRole, summary: "A reconciliation run and its items",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Recon.Get(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/reconciliation/runs/{id}/items/{itemId}/resolve", tag: tagRecon, access: operatorRole, body: resolveItemRequest{},
		summary: "Resolve a reconciliation break with a note",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			itemID, err := pathID(r, "itemId")
			if err != nil {
				return err
			}
			var req resolveItemRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Recon.Resolve(r.Context(), id, itemID, req.Note))
		})})
	s.add(route{method: "POST", path: "/ops/reconciliation/runs/{id}/settle", tag: tagRecon, access: operatorRole, status: http.StatusAccepted,
		summary: "Input the settlement (provider float → bank, net of provider fees); posted when another operator authorises it",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			change, err := s.app.Parameters.ProposeSettlement(r.Context(), id)
			return accepted(w, "/ops/parameters/pending", change, err)
		})})
}

// ---------------------------------------------------------------------------------------------
// RE — general ledger, financial statements and transaction enquiries

func (s *Server) mapGL() {
	s.add(route{method: "GET", path: "/ops/gl/chart", tag: tagGL, access: operatorRole, summary: "Chart of accounts: GL lines and the categories that consolidate into them",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Chart.Get(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/gl/balances", tag: tagGL, access: operatorRole, query: []param{asOfParam},
		summary: "GL balances by line and currency with LCY equivalent (live, or as of a closed business date)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			asOf, err := queryDate(r, "asOf")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Reports.GLBalances(r.Context(), asOf))
		}})
	s.add(route{method: "GET", path: "/ops/gl/balance-sheet", tag: tagGL, access: operatorRole, query: []param{asOfParam}, summary: "Balance sheet in LCY",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			asOf, err := queryDate(r, "asOf")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Reports.BalanceSheet(r.Context(), asOf))
		}})
	s.add(route{method: "GET", path: "/ops/gl/profit-and-loss", tag: tagGL, access: operatorRole, query: []param{asOfParam}, summary: "Profit and loss in LCY",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			asOf, err := queryDate(r, "asOf")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Reports.ProfitAndLoss(r.Context(), asOf))
		}})
	s.add(route{method: "GET", path: "/ops/gl/trial-balance", tag: tagGL, access: operatorRole, summary: "Trial balance per currency",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Reports.TrialBalance(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/gl/integrity", tag: tagGL, access: operatorRole, summary: "Recompute every balance from journal lines and report drift",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			drift, err := s.app.Reports.Integrity(r.Context())
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"healthy": len(drift) == 0, "drift": drift})
		}})
	s.add(route{method: "GET", path: "/ops/gl/internal-accounts", tag: tagGL, access: operatorRole, summary: "Internal and P&L accounts",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Reports.InternalAccounts(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/gl/journal", tag: tagGL, access: operatorRole, summary: "Transaction journal for a booking date",
		query: []param{{"date", "string", "Booking date (defaults to today)", false}, {"category", "string", "Entry category", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			date, err := queryDate(r, "date")
			if err != nil {
				return err
			}
			if date == nil {
				dates, err := s.app.Dates.Get(s.store.Read(r.Context()))
				if err != nil {
					return err
				}
				date = &dates.Today
			}
			category, err := queryEnum(r, "category", domain.EntryCategories...)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 200)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Reports.Journal(r.Context(), *date, category, limit))
		}})
	s.add(route{method: "GET", path: "/entries/{transactionReference}", tag: tagGL, access: platformRole,
		summary: "Journal entry enquiry by its T24-style reference, with every leg (the transaction record is /transactions/{reference})",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Reports.Entry(r.Context(), chi.URLParam(r, "transactionReference")))
		}})
}

// ---------------------------------------------------------------------------------------------
// Sandbox-only helpers that stand in for the outside world (a customer approving an STK push,
// a cardholder tapping a card). Never mapped in production.

func (s *Server) mapSandbox() {
	s.add(route{method: "POST", path: "/sandbox/providers/{provider}/events", tag: tagSandbox, access: platformRole, body: sandboxEventRequest{},
		summary: "Simulate a provider callback, e.g. M-Pesa STK push confirmed (charge.succeeded)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req sandboxEventRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			if req.Currency != nil {
				req.Currency = domain.Ptr(strings.ToUpper(*req.Currency))
			}
			evt := app.ProviderEvent{EventID: "evt_" + domain.RandomHex(8), Type: req.Type, Reference: req.Reference, Amount: req.Amount,
				Currency: req.Currency, FailureCode: req.FailureCode, Reason: req.Reason,
				AccountReference: req.AccountReference, Payer: req.Payer, PayerName: req.PayerName}
			outcome, err := s.app.Validations.Validate(r.Context(), chi.URLParam(r, "provider"), evt, string(mustJSON(evt)))
			if err != nil {
				return err
			}
			return ok(w, map[string]any{"eventId": evt.EventID, "outcome": outcome})
		}})
	s.add(route{method: "POST", path: "/sandbox/card-network/purchase", tag: tagSandbox, access: platformRole, body: networkAuthorizationRequest{},
		summary: "Simulate a cardholder purchase hitting the issuer authorisation endpoint",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req networkAuthorizationRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			auth, err := s.app.Cards.Authorize(r.Context(), req.command())
			if err != nil {
				return err
			}
			return ok(w, networkAuthorizationFrom(auth))
		}})
}

// ---------------------------------------------------------------------------------------------
// Handler helpers

type uuidT = uuid.UUID

// withID resolves the {id} path parameter (a malformed id is a 404, like a route constraint).
func withID(h func(w http.ResponseWriter, r *http.Request, id uuidT) error) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		return h(w, r, id)
	}
}

// respond writes a (value, error) pair as 200 JSON or an error.
func respond(w http.ResponseWriter) func(v any, err error) error {
	return func(v any, err error) error {
		if err != nil {
			return err
		}
		return ok(w, v)
	}
}

func accepted(w http.ResponseWriter, location string, v any, err error) error {
	if err != nil {
		return err
	}
	w.Header().Set("Location", location)
	return writeJSON(w, http.StatusAccepted, v)
}

func reviewReason(r *http.Request, fallback string) (string, error) {
	var req reviewRequest
	if err := decode(r, &req); err != nil {
		return "", err
	}
	return derefOr(req.Reason, fallback), nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}
