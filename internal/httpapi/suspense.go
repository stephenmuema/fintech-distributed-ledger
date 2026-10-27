package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

const (
	tagSuspense   = "AC · Suspense"
	tagExtensions = "EB · Extensions"
)

type raiseItemRequest struct {
	Type                string            `json:"type" validate:"required"`
	Currency            string            `json:"currency" validate:"required,len=3"`
	Amount              int64             `json:"amount" validate:"min=1"`
	ContraAccountNumber string            `json:"contraAccountNumber" validate:"required"`
	Reference           string            `json:"reference" validate:"max=60"`
	Narrative           string            `json:"narrative" validate:"required,max=200"`
	Attributes          map[string]string `json:"attributes"`
}

type clearingRequest struct {
	Kind                string `json:"kind" validate:"oneof=Clear|WriteOff"`
	TargetAccountNumber string `json:"targetAccountNumber" validate:"max=40"`
	Amount              int64  `json:"amount" validate:"min=1"`
	Narrative           string `json:"narrative" validate:"required,max=200"`
}

type assignRequest struct {
	AssignedTo *string `json:"assignedTo" validate:"max=80"`
}

type attributesRequest struct {
	Attributes map[string]string `json:"attributes" validate:"required"`
}

// ---------------------------------------------------------------------------------------------
// AC.SUSPENSE — suspense types, items, clearings and ageing

func (s *Server) mapSuspense() {
	s.add(route{method: "GET", path: "/ops/suspense/types", tag: tagSuspense, access: operatorRole, summary: "Suspense types (parameter records)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Suspense.Types(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/suspense/types/{code}", tag: tagSuspense, access: operatorRole, summary: "A suspense type",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Suspense.Type(r.Context(), chi.URLParam(r, "code")))
		}})
	s.add(route{method: "POST", path: "/ops/suspense/types/{code}", tag: tagSuspense, access: operatorRole, status: http.StatusAccepted, body: domain.SuspenseTypeSpec{},
		summary: "Input a new suspense type or an amendment (INAU) — accounts open when a second operator authorises",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.SuspenseTypeSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeSuspenseType(r.Context(), chi.URLParam(r, "code"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})

	s.add(route{method: "GET", path: "/ops/suspense/items", tag: tagSuspense, access: operatorRole, summary: "Suspense items with their age",
		query: []param{{"type", "string", "Suspense type code", false}, {"currency", "string", "Currency", false},
			{"status", "string", "Unauthorised | Open | PartiallyCleared | Cleared | WrittenOff | Rejected", false},
			{"open", "boolean", "Only items still in suspense (default true when no status is given)", false},
			{"assignedTo", "string", "Owner", false}, {"bucket", "string", "0-7 | 8-30 | 31-90 | 90+", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.ItemUnauthorised, domain.ItemOpen, domain.ItemPartiallyCleared, domain.ItemCleared, domain.ItemWrittenOff, domain.ItemRejected)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 200)
			if err != nil {
				return err
			}
			query := r.URL.Query()
			return respond(w)(s.app.Suspense.Items(r.Context(), app.ItemFilter{Type: query.Get("type"), Currency: query.Get("currency"), Status: status,
				OpenOnly: query.Get("open") != "false", AssignedTo: query.Get("assignedTo"), Bucket: query.Get("bucket"), Limit: limit}))
		}})
	s.add(route{method: "GET", path: "/ops/suspense/items/{id}", tag: tagSuspense, access: operatorRole,
		summary: "A suspense item with its posting, clearings, extension calls and record history",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Suspense.Item(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/suspense/items", tag: tagSuspense, access: operatorRole, idempotent: true, status: http.StatusAccepted, body: raiseItemRequest{},
		summary: "Raise a suspense item against an internal or P&L account (INAU) — posted when a second operator authorises",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req raiseItemRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			item, err := s.app.Suspense.Raise(r.Context(), app.RaiseInput{Type: req.Type, Currency: req.Currency, Amount: req.Amount,
				ContraAccountNumber: req.ContraAccountNumber, Reference: req.Reference, Narrative: req.Narrative, Attributes: req.Attributes})
			if err != nil {
				return err
			}
			return accepted(w, "/ops/suspense/items/"+item.ID.String(), item, nil)
		}})
	s.add(route{method: "POST", path: "/ops/suspense/items/{id}/authorise", tag: tagSuspense, access: operatorRole, summary: "Authorise and post a raised item (second operator)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Suspense.AuthoriseRaise(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/suspense/items/{id}/reject", tag: tagSuspense, access: operatorRole, body: reviewRequest{}, summary: "Reject a raised item",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			reason, err := reviewReason(r, "rejected")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Suspense.RejectRaise(r.Context(), id, reason))
		})})
	s.add(route{method: "POST", path: "/ops/suspense/items/{id}/assign", tag: tagSuspense, access: operatorRole, body: assignRequest{}, summary: "Assign an item to an owner (null unassigns)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req assignRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			if req.AssignedTo != nil && strings.TrimSpace(*req.AssignedTo) == "" {
				req.AssignedTo = nil
			}
			return respond(w)(s.app.Suspense.Assign(r.Context(), id, req.AssignedTo))
		})})
	s.add(route{method: "PUT", path: "/ops/suspense/items/{id}/attributes", tag: tagSuspense, access: operatorRole, body: attributesRequest{},
		summary: "Set local-field values on an item (validated against its type; empty removes)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req attributesRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			return respond(w)(s.app.Suspense.UpdateAttributes(r.Context(), id, req.Attributes))
		})})
	s.add(route{method: "POST", path: "/ops/suspense/items/{id}/clearings", tag: tagSuspense, access: operatorRole, idempotent: true, status: http.StatusAccepted, body: clearingRequest{},
		summary: "Input a clearing or write-off (INAU); the type's validators may veto it",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req clearingRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			kind, err := enumOr("kind", req.Kind, domain.ClearingClear, domain.ClearingClear, domain.ClearingWriteOff)
			if err != nil {
				return err
			}
			clearing, err := s.app.Suspense.ProposeClearing(r.Context(), id, app.ClearingInput{Kind: kind, TargetAccountNumber: strings.TrimSpace(req.TargetAccountNumber),
				Amount: req.Amount, Narrative: req.Narrative})
			return accepted(w, "/ops/suspense/clearings?status=Unauthorised", clearing, err)
		})})

	s.add(route{method: "GET", path: "/ops/suspense/clearings", tag: tagSuspense, access: operatorRole, summary: "Clearings (e.g. ?status=Unauthorised)",
		query: []param{{"status", "string", "Unauthorised | Authorised | Rejected", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.Unauthorised, domain.Authorised, domain.Rejected)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Suspense.Clearings(r.Context(), status, limit))
		}})
	s.add(route{method: "POST", path: "/ops/suspense/clearings/{id}/authorise", tag: tagSuspense, access: operatorRole,
		summary: "Authorise and post a clearing (a different party from its inputter)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Suspense.AuthoriseClearing(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/suspense/clearings/{id}/reject", tag: tagSuspense, access: operatorRole, body: reviewRequest{}, summary: "Reject a clearing",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			reason, err := reviewReason(r, "rejected")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Suspense.RejectClearing(r.Context(), id, reason))
		})})

	s.add(route{method: "GET", path: "/ops/suspense/ageing", tag: tagSuspense, access: operatorRole, summary: "Ageing per suspense account: open items by age bucket",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Suspense.Ageing(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/suspense/integrity", tag: tagSuspense, access: operatorRole,
		summary: "Suspense accounts whose balance disagrees with their open items (empty when healthy)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Suspense.Integrity(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/suspense/run-hooks", tag: tagSuspense, access: operatorRole,
		summary: "Classify and match open items through their types' extensions now (COB does this every night)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Suspense.RunHooks(r.Context()))
		}})
}

// ---------------------------------------------------------------------------------------------
// EB.API — extensions

func (s *Server) mapExtensions() {
	s.add(route{method: "GET", path: "/core/extensions", tag: tagExtensions, access: operatorRole,
		summary: "In-process and remote extensions, the hooks they serve and their circuit state",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Extensions.List(r.Context()))
		}})
	s.add(route{method: "GET", path: "/core/extensions/hooks", tag: tagExtensions, access: operatorRole, summary: "The hook points extensions can serve",
		handle: func(w http.ResponseWriter, r *http.Request) error { return ok(w, app.KnownHooks()) }})
	s.add(route{method: "GET", path: "/core/extensions/calls", tag: tagExtensions, access: operatorRole, summary: "Extension call log, newest first",
		query: []param{{"extension", "string", "Extension name", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Extensions.Calls(r.Context(), r.URL.Query().Get("extension"), limit))
		}})
	s.add(route{method: "POST", path: "/ops/parameters/hooks/{name}", tag: tagExtensions, access: operatorRole, status: http.StatusAccepted, body: domain.HookEndpointSpec{},
		summary: "Input a remote extension endpoint or an amendment (INAU); the shared secret is named, never sent",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.HookEndpointSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeEndpoint(r.Context(), chi.URLParam(r, "name"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
}
