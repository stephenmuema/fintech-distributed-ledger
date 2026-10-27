package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// problem is an RFC 9457 problem detail with a stable, machine-readable code.
type problem struct {
	Type       string              `json:"type"`
	Title      string              `json:"title"`
	Status     int                 `json:"status"`
	Detail     string              `json:"detail,omitempty"`
	Instance   string              `json:"instance,omitempty"`
	Code       string              `json:"code"`
	TraceID    string              `json:"traceId,omitempty"`
	Overrides  []domain.Override   `json:"overrides,omitempty"`
	Errors     map[string][]string `json:"errors,omitempty"`
	TransferID string              `json:"transferId,omitempty"`
}

func newProblem(r *http.Request, status int, code, detail string) *problem {
	return &problem{
		Type: "https://zirnis.dev/errors/" + code, Title: code, Status: status, Detail: detail,
		Instance: r.URL.Path, Code: code, TraceID: correlationID(r),
	}
}

func writeProblem(w http.ResponseWriter, p *problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// apiError is a request-level failure (bad input) raised by handlers.
type apiError struct {
	status int
	code   string
	detail string
	errors map[string][]string
}

func (e *apiError) Error() string { return e.detail }

func badRequest(code, format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, code: code, detail: fmt.Sprintf(format, args...)}
}

func notFound(resource string, id any) error { return domain.NotFound(resource, id) }

// forbidden is a 403 problem.
func forbidden(code, detail string) error {
	return &apiError{status: http.StatusForbidden, code: code, detail: detail}
}

// fail maps an error to a problem response:
// 400 bad input · 404 not found · 409 conflict / overrides · 422 business rule · 500 unexpected.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var p *problem
	var apiErr *apiError
	switch derr, isDomain := domain.AsError(err); {
	case errors.As(err, &apiErr):
		p = newProblem(r, apiErr.status, apiErr.code, apiErr.detail)
		if apiErr.errors != nil {
			p.Title = "One or more validation errors occurred."
			p.Errors = apiErr.errors
		}
	case isDomain:
		status := map[domain.ErrorKind]int{domain.KindRule: 422, domain.KindNotFound: 404, domain.KindConflict: 409}[derr.Kind]
		p = newProblem(r, status, derr.Code, derr.Message)
		p.Overrides = derr.Overrides
	case errors.Is(err, store.ErrConcurrency):
		p = newProblem(r, http.StatusConflict, "concurrency_conflict", "The resource was modified concurrently. Please retry.")
	default:
		s.logger.Error("unhandled error", "path", r.URL.Path, "error", err, "correlationId", correlationID(r))
		s.auditFailure(r, http.StatusInternalServerError, err)
		p = newProblem(r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred.")
	}
	if p.Status < 500 {
		s.logger.Info("request rejected", "code", p.Code, "detail", p.Detail, "path", r.URL.Path)
	}
	writeProblem(w, p)
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body (an empty body counts as {}) and validates it.
func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return badRequest("bad_request", "Could not read the request body.")
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, v); err != nil {
			return badRequest("bad_request", "Malformed JSON: %s", err.Error())
		}
	}
	return validate(v)
}

var (
	phonePattern = regexp.MustCompile(`^\+?[0-9 ()-]{7,20}$`)
)

// validate checks `validate:"..."` struct tags: required, min=, max=, len=, email, url, phone,
// oneof=A|B (case-insensitive). min/max apply to string length, slice length or numeric value.
func validate(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil
	}
	errs := map[string][]string{}
	t := rv.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		rules := f.Tag.Get("validate")
		if rules == "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		value := rv.Field(i)
		present := !(value.Kind() == reflect.Pointer && value.IsNil())
		if value.Kind() == reflect.Pointer && present {
			value = value.Elem()
		}
		for _, rule := range strings.Split(rules, ",") {
			key, arg, _ := strings.Cut(rule, "=")
			if msg := check(key, arg, value, present); msg != "" {
				errs[name] = append(errs[name], fmt.Sprintf("The %s field %s", name, msg))
				break
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &apiError{status: http.StatusBadRequest, code: "validation_failed", detail: "One or more validation errors occurred.", errors: errs}
}

// validateNested validates a request and the nested objects it carries (validate is flat),
// naming a nested field by its path, e.g. "customer.email".
func validateNested(v any, nested map[string]any) error {
	errs := map[string][]string{}
	collect := func(prefix string, v any) {
		var apiErr *apiError
		if err := validate(v); errors.As(err, &apiErr) {
			for field, messages := range apiErr.errors {
				errs[prefix+field] = messages
			}
		}
	}
	collect("", v)
	for name, n := range nested {
		if rv := reflect.ValueOf(n); rv.Kind() == reflect.Pointer && !rv.IsNil() {
			collect(name+".", n)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &apiError{status: http.StatusBadRequest, code: "validation_failed", detail: "One or more validation errors occurred.", errors: errs}
}

func check(rule, arg string, v reflect.Value, present bool) string {
	n, _ := strconv.ParseInt(arg, 10, 64)
	switch rule {
	case "required":
		if !present || (v.Kind() == reflect.String && strings.TrimSpace(v.String()) == "") || (v.Kind() == reflect.Slice && v.Len() == 0) {
			return "is required."
		}
		return ""
	}
	if !present || (v.Kind() == reflect.String && v.String() == "" && rule != "len") {
		return "" // optional and absent
	}
	switch rule {
	case "min", "max":
		var size int64
		switch v.Kind() {
		case reflect.String:
			size = int64(len([]rune(v.String())))
		case reflect.Slice:
			size = int64(v.Len())
		case reflect.Int, reflect.Int64, reflect.Int32:
			size = v.Int()
		default:
			return ""
		}
		if rule == "min" && size < n {
			return "must be at least " + arg + "."
		}
		if rule == "max" && size > n {
			return "must be at most " + arg + "."
		}
	case "len":
		if int64(len([]rune(v.String()))) != n {
			return "must be exactly " + arg + " characters."
		}
	case "email":
		if addr, err := mail.ParseAddress(v.String()); err != nil || addr.Address != v.String() {
			return "is not a valid e-mail address."
		}
	case "url":
		if u, err := url.Parse(v.String()); err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
			return "is not a valid fully-qualified http or https URL."
		}
	case "phone":
		if !phonePattern.MatchString(v.String()) {
			return "is not a valid phone number."
		}
	case "oneof":
		for _, option := range strings.Split(arg, "|") {
			if strings.EqualFold(option, v.String()) {
				return ""
			}
		}
		return "must be one of " + strings.ReplaceAll(arg, "|", ", ") + "."
	}
	return ""
}

func logRecovered(logger *slog.Logger, r *http.Request, p any) {
	logger.Error("panic serving request", "path", r.URL.Path, "panic", p, "correlationId", correlationID(r))
}

// auditFailure records a server-side failure in the audit trail with its correlation id, so a
// request that broke mid-flight can be traced alongside the ledger and provider calls it touched.
func (s *Server) auditFailure(r *http.Request, status int, cause error) {
	client := "anonymous"
	if p := principalOf(r); p != nil && !p.Invalid {
		client = p.Name
	}
	message := cause.Error()
	if len(message) > 500 {
		message = message[:500]
	}
	err := s.store.Atomic(context.WithoutCancel(r.Context()), func(tx *store.Tx) error {
		return tx.Audit("api.request_failed", "http_request", correlationID(r), map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": status, "client": client, "error": message})
	})
	if err != nil {
		s.logger.Error("could not audit failed request", "error", err)
	}
}
