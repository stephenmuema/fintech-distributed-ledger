package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

const tagChannels = "CH · Provider channels"

// ---------------------------------------------------------------------------------------------
// CH — provider channels: onboarding, transfers and the extension callbacks

func (s *Server) mapChannels() {
	s.add(route{method: "GET", path: "/ops/channels", tag: tagChannels, access: operatorRole, summary: "Provider channels",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Channels.List(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/channels/{code}", tag: tagChannels, access: operatorRole,
		summary: "A channel with its inward, outward and settlement accounts and event health",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Channels.Get(r.Context(), chi.URLParam(r, "code")))
		}})
	s.add(route{method: "POST", path: "/ops/channels/{code}", tag: tagChannels, access: operatorRole, status: http.StatusAccepted, body: domain.ChannelSpec{},
		summary: "Onboard a provider or amend it (INAU): authorising opens its inward/outward suspense accounts and settlement accounts and binds its module",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.ChannelSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeChannel(r.Context(), chi.URLParam(r, "code"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/channels/{code}/transfers", tag: tagChannels, access: operatorRole, summary: "A channel's transfers, newest first",
		query: []param{limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Channels.Transfers(r.Context(), chi.URLParam(r, "code"), limit))
		}})

	s.add(route{method: "GET", path: "/ops/suspense/events", tag: tagSuspense, access: operatorRole,
		summary: "Events that fire extensions when funds land in suspense (e.g. ?status=Failed)",
		query:   []param{{"status", "string", "Pending | Delivering | Delivered | Held | Failed", false}, limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			status, err := queryEnum(r, "status", domain.EventPending, domain.EventDelivering, domain.EventDelivered, domain.EventHeld, domain.EventFailed)
			if err != nil {
				return err
			}
			limit, err := queryInt(r, "limit", 100)
			if err != nil {
				return err
			}
			return respond(w)(s.app.Suspense.Events(r.Context(), status, limit))
		}})
	s.add(route{method: "POST", path: "/ops/suspense/events/{id}/retry", tag: tagSuspense, access: operatorRole,
		summary: "Deliver an event again now (a failed one once fixed, or a held one to ask the provider again)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Suspense.RetryEvent(r.Context(), id))
		})})

	// Callbacks from remote extensions, signed with the extension's shared secret. The extension
	// acts as maker ("hook:<name>"); the core still decides.
	s.add(route{method: "POST", path: "/webhooks/extensions/{name}/channels/{code}/inward", tag: tagChannels, access: public, dedupedBy: "providerReference",
		body: extapi.InwardFunds{}, summary: "Extension callback: book money a provider sent into the channel's inward suspense account",
		handle: s.extensionCallback(func(w http.ResponseWriter, r *http.Request, name string, body []byte) error {
			var funds extapi.InwardFunds
			if err := json.Unmarshal(body, &funds); err != nil {
				return badRequest("bad_request", "Malformed JSON: %s", err.Error())
			}
			t, err := s.app.Channels.Inward(r.Context(), chi.URLParam(r, "code"), name, funds)
			return respond(w)(t, err)
		})})
	s.add(route{method: "POST", path: "/webhooks/extensions/{name}/items/{id}/outcome", tag: tagChannels, access: public, dedupedBy: "item id (an item settles once)",
		body: extapi.FundsOutcome{}, summary: "Extension callback: report the outcome (complete, return, hold) of funds in suspense",
		handle: s.extensionCallback(func(w http.ResponseWriter, r *http.Request, name string, body []byte) error {
			id, err := pathID(r, "id")
			if err != nil {
				return err
			}
			var outcome extapi.FundsOutcome
			if err := json.Unmarshal(body, &outcome); err != nil {
				return badRequest("bad_request", "Malformed JSON: %s", err.Error())
			}
			item, err := s.app.Suspense.ApplyOutcome(r.Context(), id, name, outcome)
			if err != nil {
				return err
			}
			if t, err := s.app.Channels.Transfer(r.Context(), item.ID); err == nil {
				return ok(w, t)
			}
			return ok(w, item)
		})})
}

// extensionCallback verifies a callback from a registered, enabled remote extension against its
// shared secret before the handler runs, and makes the extension the acting party.
func (s *Server) extensionCallback(h func(w http.ResponseWriter, r *http.Request, name string, body []byte) error) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		name := chi.URLParam(r, "name")
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return badRequest("bad_request", "Could not read the request body.")
		}
		endpoint, err := store.Get[domain.HookEndpoint](s.store.Read(r.Context()), "SELECT * FROM hook_endpoints WHERE name = ?", name)
		if err != nil {
			return err
		}
		if endpoint == nil || !endpoint.Enabled || !extapi.Verify(r.Header.Get(extapi.SignatureHeader), os.Getenv(endpoint.SecretEnv), body, time.Now()) {
			writeProblem(w, newProblem(r, http.StatusUnauthorized, "invalid_signature", "The callback is not signed by a registered extension."))
			return nil
		}
		return h(w, r.WithContext(store.WithActor(r.Context(), "hook:"+name)), name, body)
	}
}
