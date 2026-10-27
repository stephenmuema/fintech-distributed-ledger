package httpapi

import (
	"encoding/csv"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

const tagEnquiries = "EB · Enquiries"

// ---------------------------------------------------------------------------------------------
// ENQUIRY — saved queries defined by the bank, run by the core

func (s *Server) mapEnquiries() {
	s.add(route{method: "GET", path: "/ops/enquiries", tag: tagEnquiries, access: operatorRole, summary: "Enquiries (parameter records)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Enquiries.List(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/enquiries/{name}", tag: tagEnquiries, access: operatorRole, summary: "An enquiry",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Enquiries.Get(r.Context(), chi.URLParam(r, "name")))
		}})
	s.add(route{method: "POST", path: "/ops/enquiries/{name}", tag: tagEnquiries, access: operatorRole, status: http.StatusAccepted, body: domain.EnquirySpec{},
		summary: "Input an enquiry or an amendment (INAU)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.EnquirySpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeEnquiry(r.Context(), chi.URLParam(r, "name"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "GET", path: "/ops/enquiries/{name}/run", tag: tagEnquiries, access: operatorRole,
		summary: "Run an enquiry; each query parameter fills the prompt for that field (RG takes from,to; IN takes a,b,c)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Enquiries.Run(r.Context(), chi.URLParam(r, "name"), selections(r)))
		}})
	s.add(route{method: "GET", path: "/ops/enquiries/{name}/run.csv", tag: tagEnquiries, access: operatorRole,
		summary: "Run an enquiry and download the result as CSV",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			result, err := s.app.Enquiries.Run(r.Context(), chi.URLParam(r, "name"), selections(r))
			if err != nil {
				return err
			}
			return writeEnquiryCSV(w, result)
		}})
}

func selections(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func writeEnquiryCSV(w http.ResponseWriter, result *app.EnquiryResult) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, result.Enquiry))
	out := csv.NewWriter(w)
	header := make([]string, len(result.Columns))
	for i, c := range result.Columns {
		header[i] = firstNonEmpty(c.Label, c.Field)
	}
	if err := out.Write(header); err != nil {
		return err
	}
	for _, row := range result.Rows {
		record := make([]string, len(result.Columns))
		for i, c := range result.Columns {
			if v := row[c.Field]; v != nil {
				record[i] = csvSafe(fmt.Sprint(v))
			}
		}
		if err := out.Write(record); err != nil {
			return err
		}
	}
	out.Flush()
	return out.Error()
}

// csvSafe defuses spreadsheet formulas in exported values (CSV injection).
func csvSafe(s string) string {
	if s != "" && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		if _, err := fmt.Sscan(s, new(float64)); err != nil {
			return "'" + s
		}
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
