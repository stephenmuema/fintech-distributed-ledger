package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

const tagLocal = "EB · Local tables"

type localRecordRequest struct {
	Values map[string]string `json:"values" validate:"required"`
}

// ---------------------------------------------------------------------------------------------
// EB.LOCAL — local tables (definitions under maker-checker, DDL on authorisation) and records

func (s *Server) mapLocalTables() {
	s.add(route{method: "GET", path: "/ops/local-tables", tag: tagLocal, access: operatorRole,
		summary: "Local table definitions with the state of their columns",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.LocalTables.List(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/local-tables/{name}", tag: tagLocal, access: operatorRole, summary: "A local table definition",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.LocalTables.Get(r.Context(), chi.URLParam(r, "name")))
		}})
	s.add(route{method: "POST", path: "/ops/local-tables/{name}", tag: tagLocal, access: operatorRole, status: http.StatusAccepted, body: domain.LocalTableSpec{},
		summary: "Input a local table or an amendment (INAU) — its columns are added when a second operator authorises",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.LocalTableSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeLocalTable(r.Context(), chi.URLParam(r, "name"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/local-tables/{name}/ddl", tag: tagLocal, access: operatorRole,
		summary: "The SQL still to run for a local table, for this database engine (for a DBA under ZIRNIS_LOCAL_DDL=manual)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			statements, err := s.app.LocalTables.Statements(r.Context(), chi.URLParam(r, "name"))
			return respond(w)(map[string]any{"engine": s.store.Dialect(), "statements": statements}, err)
		}})
	s.add(route{method: "POST", path: "/ops/local-tables/{name}/apply", tag: tagLocal, access: operatorRole,
		summary: "Run a local table's outstanding DDL now (or, in manual mode, pick up what a DBA ran)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.LocalTables.Apply(r.Context(), chi.URLParam(r, "name")))
		}})

	s.add(route{method: "GET", path: "/ops/local/{name}", tag: tagLocal, access: operatorRole,
		summary: "A local application's records; any other query parameter selects on a searchable field",
		query:   []param{limitParam},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			limit, err := queryInt(r, "limit", 200)
			if err != nil {
				return err
			}
			fields := map[string]string{}
			for k, v := range r.URL.Query() {
				if k != "limit" && len(v) > 0 {
					fields[k] = v[0]
				}
			}
			return respond(w)(s.app.LocalTables.Records(r.Context(), chi.URLParam(r, "name"), app.RecordFilter{Fields: fields, Limit: limit}))
		}})
	s.add(route{method: "GET", path: "/ops/local/{name}/{key}", tag: tagLocal, access: operatorRole,
		summary: "A record's local values: a customer (by id), an account (by number) or an application record (by key)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.LocalTables.Record(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "key")))
		}})
	s.add(route{method: "POST", path: "/ops/local/{name}/{key}", tag: tagLocal, access: operatorRole, body: localRecordRequest{},
		summary: "Input local values for a record — held for a checker (202) unless the table has authorisers 0 (200)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req localRecordRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			name, key := chi.URLParam(r, "name"), chi.URLParam(r, "key")
			change, record, err := s.app.Parameters.ProposeLocalRecord(r.Context(), name, key, req.Values)
			if err != nil {
				return err
			}
			if change != nil {
				return accepted(w, "/ops/parameters/pending", change, nil)
			}
			return ok(w, record)
		}})
}
