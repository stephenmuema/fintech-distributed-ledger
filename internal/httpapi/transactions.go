package httpapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

const tagTxn = "FT · Transactions"

// transactionRequest is a transaction as input on a version. Which fields a version takes
// depends on its type's flow (GET /versions shows each screen).
type transactionRequest struct {
	Version        string   `json:"version" validate:"required,max=40"`
	DebitAccount   string   `json:"debitAccount" validate:"max=34"`
	CreditAccount  string   `json:"creditAccount" validate:"max=34"`
	Amount         *int64   `json:"amount" validate:"min=1"`
	Currency       string   `json:"currency" validate:"max=3"`
	Narrative      string   `json:"narrative" validate:"max=140"`
	PaymentDetails []string `json:"paymentDetails"`
	// Collections: the method, the card token / MSISDN / "hosted_checkout", the payer and where
	// a hosted checkout returns.
	Method    string        `json:"method" validate:"max=20"`
	Source    string        `json:"source" validate:"max=120"`
	Payer     *payerRequest `json:"payer"`
	ReturnURL *string       `json:"returnUrl" validate:"max=500"`
	// Payouts: the rail and the beneficiary.
	Rail        string              `json:"rail" validate:"max=20"`
	Beneficiary *beneficiaryRequest `json:"beneficiary"`
	// Channel transfers: the channel, its reference and the channel's own fields.
	ChannelCode string            `json:"channelCode" validate:"max=20"`
	Reference   string            `json:"reference" validate:"max=60"`
	Attributes  map[string]string `json:"attributes"`
	// Refunds: the collection's transaction reference.
	OriginalReference string `json:"originalReference" validate:"max=20"`
	Reason            string `json:"reason" validate:"max=200"`
	// Local are the bank's local fields of the transaction (lt_transaction).
	Local             map[string]string `json:"local"`
	AcceptedOverrides []string          `json:"acceptedOverrides"`
}

// payerRequest is the payer of a collection (all of it for a hosted checkout). Only the name and
// phone are kept on the transaction; the rest goes to the provider's page.
type payerRequest struct {
	FirstName  string `json:"firstName" validate:"max=60"`
	LastName   string `json:"lastName" validate:"max=60"`
	Email      string `json:"email" validate:"max=120"`
	Phone      string `json:"phone" validate:"max=20"`
	Address    string `json:"address" validate:"max=120"`
	PostalCode string `json:"postalCode" validate:"max=20"`
}

type beneficiaryRequest struct {
	Account  string `json:"account" validate:"max=34"`
	Name     string `json:"name" validate:"max=140"`
	BankCode string `json:"bankCode" validate:"max=20"`
}

// fields flattens a request into the core field paths a version is defined on.
func (req transactionRequest) fields() map[string]string {
	f := map[string]string{"debitAccount": req.DebitAccount, "creditAccount": req.CreditAccount, "currency": strings.ToUpper(req.Currency),
		"narrative": req.Narrative, "method": req.Method, "source": req.Source, "rail": req.Rail, "channelCode": req.ChannelCode,
		"reference": req.Reference, "originalReference": req.OriginalReference, "reason": req.Reason}
	if req.Amount != nil {
		f["amount"] = strconv.FormatInt(*req.Amount, 10)
	}
	if req.ReturnURL != nil {
		f["returnUrl"] = *req.ReturnURL
	}
	if p := req.Payer; p != nil {
		f["payer.firstName"], f["payer.lastName"], f["payer.email"] = p.FirstName, p.LastName, p.Email
		f["payer.phone"], f["payer.address"], f["payer.postalCode"] = p.Phone, p.Address, p.PostalCode
	}
	if b := req.Beneficiary; b != nil {
		f["beneficiary.account"], f["beneficiary.name"], f["beneficiary.bankCode"] = b.Account, b.Name, b.BankCode
	}
	for k, v := range f {
		if strings.TrimSpace(v) == "" {
			delete(f, k)
		}
	}
	return f
}

// inputChannel: operators transact on the console channel (their versions need a checker
// outside the sandbox), API clients on the api channel.
func inputChannel(p *principal) string {
	if p != nil && (p.Session != "" || slices.Contains(p.Roles, "operator")) {
		return domain.ChannelConsole
	}
	return domain.ChannelAPI
}

// ---------------------------------------------------------------------------------------------
// FT — transactions, transaction types and versions

func (s *Server) mapTransactions() {
	s.add(route{method: "POST", path: "/transactions", tag: tagTxn, access: platformRole, idempotent: true, consoleAllowed: true, status: http.StatusCreated,
		body:    transactionRequest{},
		summary: "Input a transaction on a version: 201 when it executed straight through, 202 when it waits for a checker (INAU); 409 lists overrides to accept",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req transactionRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			p := principalOf(r)
			channel := inputChannel(p)
			if channel == domain.ChannelConsole && !p.can(domain.PermMaker) {
				return forbidden("permission_required", "Inputting a transaction needs the maker permission; ask an administrator.")
			}
			if req.ReturnURL != nil {
				checked, err := s.returnURL(req.ReturnURL)
				if err != nil {
					return err
				}
				req.ReturnURL = checked
			}
			view, err := s.app.Transactions.Input(r.Context(), app.InputTransaction{Version: req.Version, Channel: channel, Fields: req.fields(),
				PaymentDetails: req.PaymentDetails, Attributes: req.Attributes, Local: req.Local, AcceptedOverrides: req.AcceptedOverrides})
			if err != nil {
				return err
			}
			return transactionResult(w, view)
		}})
	s.add(route{method: "GET", path: "/transactions", tag: tagTxn, access: platformRole, summary: "Transactions, newest first",
		query: []param{{"type", "string", "Transaction type", false}, {"version", "string", "Version", false}, {"account", "string", "Debit or credit account", false},
			{"channel", "string", "api | console | link | system", false}, {"recordStatus", "string", "INAU | LIVE | RJCT", false},
			{"status", "string", "Pending | Processing | Completed | Failed | Reversed", false},
			{"from", "string", "Booking date from (YYYY-MM-DD)", false}, {"to", "string", "Booking date to", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			f, err := transactionFilter(r)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Transactions.List(r.Context(), f))
		}})
	s.add(route{method: "GET", path: "/transactions/{reference}", tag: tagTxn, access: platformRole,
		summary: "A transaction with its local fields, payment details, journal entries and flow record",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(txnJSON(s.app.Transactions.Get(r.Context(), chi.URLParam(r, "reference"))))
		}})
	s.add(route{method: "GET", path: "/ops/transactions/pending", tag: tagTxn, access: operatorRole, summary: "Transactions waiting for a checker (INAU)",
		query: []param{limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 200)
			if err != nil {
				return err
			}
			status := domain.RecordUnauthorised
			return respond(w)(s.app.Transactions.List(r.Context(), app.TransactionFilter{RecordStatus: &status, Limit: limit}))
		}})
	s.add(route{method: "POST", path: "/transactions/{reference}/cancel", tag: tagTxn, access: platformRole, body: reviewRequest{},
		summary: "Cancel a transaction still waiting for a checker, at the request of the client (and customer) that input it; nothing had moved",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req reviewRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Transactions.Cancel(r.Context(), chi.URLParam(r, "reference"), derefOr(req.Reason, "")))
		}})
	s.add(route{method: "POST", path: "/transactions/{reference}/authorise", tag: tagTxn, access: operatorRole,
		summary: "Authorise an INAU transaction (second operator): it is re-validated and executes now",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(txnJSON(s.app.Transactions.Authorise(r.Context(), chi.URLParam(r, "reference"))))
		}})
	s.add(route{method: "POST", path: "/transactions/{reference}/reject", tag: tagTxn, access: operatorRole, body: reviewRequest{},
		summary: "Reject an INAU transaction; nothing moves",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			reason, err := reviewReason(r, "rejected")
			if err != nil {
				return err
			}
			return respond(w)(txnJSON(s.app.Transactions.Reject(r.Context(), chi.URLParam(r, "reference"), reason)))
		}})

	s.add(route{method: "GET", path: "/versions", tag: tagTxn, access: platformRole,
		summary: "Versions (screens) with the fields each takes; ?channel=api|console narrows to those you can input on",
		query:   []param{{"channel", "string", "api | console | link | system", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Transactions.Versions(r.Context(), r.URL.Query().Get("channel")))
		}})
	s.add(route{method: "GET", path: "/versions/{name}", tag: tagTxn, access: platformRole, summary: "A version with its screen",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Transactions.VersionByName(r.Context(), chi.URLParam(r, "name")))
		}})
	s.add(route{method: "POST", path: "/ops/versions/{name}", tag: tagTxn, access: operatorRole, status: http.StatusAccepted, body: domain.VersionSpec{},
		summary: "Input a version or an amendment (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.VersionSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeVersion(r.Context(), chi.URLParam(r, "name"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/transaction-types", tag: tagTxn, access: operatorRole, summary: "Transaction types (parameter records)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Transactions.Types(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/transaction-types/{code}", tag: tagTxn, access: operatorRole, summary: "A transaction type",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Transactions.Type(r.Context(), chi.URLParam(r, "code")))
		}})
	s.add(route{method: "POST", path: "/ops/transaction-types/{code}", tag: tagTxn, access: operatorRole, status: http.StatusAccepted, body: domain.TransactionTypeSpec{},
		summary: "Input a transaction type or an amendment (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.TransactionTypeSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeTransactionType(r.Context(), chi.URLParam(r, "code"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
}

// txnJSON renders a transaction's flow record in the API's shape.
func txnJSON(v *app.TxnView, err error) (*app.TxnView, error) {
	if err != nil {
		return nil, err
	}
	switch f := v.Flow.(type) {
	case *domain.PaymentIntent:
		v.Flow = paymentFrom(f)
	case *domain.Payout:
		v.Flow = payoutFrom(f)
	}
	return v, nil
}

func transactionResult(w http.ResponseWriter, view *app.TxnView) error {
	view, _ = txnJSON(view, nil)
	location := "/transactions/" + view.Reference
	if view.RecordStatus == domain.RecordUnauthorised {
		return accepted(w, location, view, nil)
	}
	return created(w, location, view)
}

func transactionFilter(r *http.Request) (app.TransactionFilter, error) {
	q := r.URL.Query()
	f := app.TransactionFilter{Type: q.Get("type"), Version: q.Get("version"), Account: q.Get("account"), Channel: q.Get("channel")}
	var err error
	if f.Limit, err = queryInt(r, "limit", 100); err != nil {
		return f, err
	}
	if f.RecordStatus, err = queryEnum(r, "recordStatus", domain.RecordUnauthorised, domain.RecordLive, domain.RecordRejected); err != nil {
		return f, err
	}
	if f.Status, err = queryEnum(r, "status", domain.TxnPending, domain.TxnProcessing, domain.TxnCompleted, domain.TxnFailed, domain.TxnReversed); err != nil {
		return f, err
	}
	for name, target := range map[string]**domain.Date{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			d, err := domain.ParseDate(v)
			if err != nil {
				return f, badRequest("validation_failed", "Query parameter '%s' must be a date (YYYY-MM-DD).", name)
			}
			*target = &d
		}
	}
	return f, nil
}
