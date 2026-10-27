package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

const tagNostro = "NV · Nostro & vostro"

type statementRequest struct {
	Format          string `json:"format" validate:"oneof=csv|mt940|camt053"`
	Content         string `json:"content" validate:"required"`
	OpeningBalance  *int64 `json:"openingBalance"`
	ClosingBalance  *int64 `json:"closingBalance"`
	StatementNumber string `json:"statementNumber" validate:"max=35"`
}

type manualMatchRequest struct {
	StatementLineIDs []uuid.UUID `json:"statementLineIds"`
	BookLineIDs      []uuid.UUID `json:"bookLineIds"`
}

type vostroResponse struct {
	accountResponse
	HolderName     string `json:"holderName"`
	CustomerNumber int64  `json:"customerNumber"`
	Correspondent  string `json:"correspondent"`
}

// ---------------------------------------------------------------------------------------------
// NV — correspondents, nostro reconciliation and vostro statements

func (s *Server) mapCorrespondents() {
	s.add(route{method: "GET", path: "/ops/correspondents", tag: tagNostro, access: operatorRole, summary: "Correspondent banks, PSPs and MNOs",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Nostro.Correspondents(r.Context()))
		}})
	s.add(route{method: "POST", path: "/ops/correspondents/{code}", tag: tagNostro, access: operatorRole, status: http.StatusAccepted, body: domain.CorrespondentSpec{},
		summary: "Input a correspondent or an amendment (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.CorrespondentSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeCorrespondent(r.Context(), chi.URLParam(r, "code"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})

	s.add(route{method: "GET", path: "/ops/nostros", tag: tagNostro, access: operatorRole,
		summary: "Nostro accounts: book balance, latest statement, open items and (with ?live=true) the correspondent's live balance",
		query:   []param{{"live", "boolean", "Ask connectors that support it for the live balance", false}},
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Nostro.Accounts(r.Context(), r.URL.Query().Get("live") == "true"))
		}})
	s.add(route{method: "POST", path: "/ops/nostros", tag: tagNostro, access: operatorRole, status: http.StatusAccepted, body: domain.NostroSpec{},
		summary: "Input a nostro account or an amendment (INAU); a new nostro opens or adopts its ledger account when authorised",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.NostroSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeNostro(r.Context(), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/nostros/{id}", tag: tagNostro, access: operatorRole, summary: "A nostro with its position",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Nostro.Get(r.Context(), id))
		})})
	s.add(route{method: "GET", path: "/ops/nostros/{id}/statements", tag: tagNostro, access: operatorRole, summary: "A nostro's statements, newest first",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Nostro.Statements(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/nostros/{id}/statements", tag: tagNostro, access: operatorRole, status: http.StatusCreated, body: statementRequest{},
		summary: "Receive a statement (MT940/MT950, camt.053 or CSV): stored, reconciled and proved; the same file is accepted once",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req statementRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			detail, err := s.app.Nostro.Ingest(r.Context(), id, app.StatementUpload{Format: req.Format, Content: req.Content,
				OpeningBalance: req.OpeningBalance, ClosingBalance: req.ClosingBalance, StatementNumber: req.StatementNumber})
			if err != nil {
				return err
			}
			return created(w, "/ops/nostro-statements/"+detail.Statement.ID.String(), detail)
		})})
	s.add(route{method: "GET", path: "/ops/nostro-statements/{id}", tag: tagNostro, access: operatorRole,
		summary: "A statement: its lines, matches, the nostro's open book entries and the reconciliation proof",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			return respond(w)(s.app.Nostro.Statement(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/nostros/{id}/reconcile", tag: tagNostro, access: operatorRole,
		summary: "Match open statement lines now: by reference, by amount and value date, then through the nostro's extensions",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			run, err := s.app.Nostro.Reconcile(r.Context(), id)
			if err != nil {
				return err
			}
			return respond(w)(run, s.app.Nostro.RefreshProofs(r.Context(), id))
		})})
	s.add(route{method: "POST", path: "/ops/nostros/{id}/matches", tag: tagNostro, access: operatorRole, status: http.StatusCreated, body: manualMatchRequest{},
		summary: "Match statement lines and book entries by hand (money in and out must agree exactly)",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			var req manualMatchRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			m, err := s.app.Nostro.MatchManually(r.Context(), id, req.StatementLineIDs, req.BookLineIDs)
			if err != nil {
				return err
			}
			return writeJSON(w, http.StatusCreated, m)
		})})
	s.add(route{method: "POST", path: "/ops/nostros/{id}/matches/{matchId}/unmatch", tag: tagNostro, access: operatorRole, summary: "Undo a match",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			matchID, err := pathID(r, "matchId")
			if err != nil {
				return err
			}
			if err := s.app.Nostro.Unmatch(r.Context(), id, matchID); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		})})
	s.add(route{method: "POST", path: "/ops/nostros/{id}/lines/{lineId}/park", tag: tagNostro, access: operatorRole,
		summary: "Park an unexplained statement line to the nostro's suspense type now",
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			lineID, err := pathID(r, "lineId")
			if err != nil {
				return err
			}
			return respond(w)(s.app.Nostro.Park(r.Context(), id, lineID))
		})})

	s.add(route{method: "GET", path: "/ops/vostros", tag: tagNostro, access: operatorRole, summary: "Vostro accounts held with us by other banks and PSPs",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			views, err := s.app.Vostro.Accounts(r.Context())
			if err != nil {
				return err
			}
			out := make([]vostroResponse, len(views))
			for i, v := range views {
				out[i] = vostroResponse{accountFrom(v.AccountView), v.HolderName, v.CustomerNumber, v.Correspondent}
			}
			return ok(w, out)
		}})
	s.add(route{method: "GET", path: "/accounts/{id}/statement/export", tag: tagNostro, access: platformRole,
		summary: "Account statement for a period as MT940 or CSV — what a vostro holder reconciles against",
		query: []param{{"from", "string", "First booking date (YYYY-MM-DD)", true}, {"to", "string", "Last booking date (YYYY-MM-DD)", true},
			{"format", "string", "mt940 | csv (default mt940)", false}},
		handle: withID(func(w http.ResponseWriter, r *http.Request, id uuidT) error {
			from, err := queryDate(r, "from")
			if err != nil {
				return err
			}
			to, err := queryDate(r, "to")
			if err != nil {
				return err
			}
			if from == nil || to == nil {
				return badRequest("bad_request", "Query parameters 'from' and 'to' are required.")
			}
			file, err := s.app.Vostro.Export(r.Context(), id, *from, *to, r.URL.Query().Get("format"))
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", file.ContentType)
			w.Header().Set("Content-Disposition", `attachment; filename="`+file.FileName+`"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(file.Body)))
			_, err = w.Write(file.Body)
			return err
		})})
}
