package httpapi

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/domain"
)

// The OpenAPI 3.0 document is generated from the route table, with request schemas reflected
// from the request types (json names, types and validate tags) — one source of truth for
// routing, validation and documentation. Scanners (OWASP ZAP) are driven from it in CI.

var pathParam = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

func (s *Server) openAPI() []byte {
	s.openapiOnce.Do(func() {
		paths := map[string]map[string]any{}
		for _, rt := range s.routes {
			op := map[string]any{
				"tags":        []string{rt.tag},
				"summary":     rt.summary,
				"operationId": operationID(rt.method, rt.path),
				"responses":   responses(rt),
			}
			var params []map[string]any
			for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
				schema := map[string]any{"type": "string"}
				if m[1] == "id" || strings.HasSuffix(m[1], "Id") {
					schema["format"] = "uuid"
				}
				params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": schema})
			}
			for _, q := range rt.query {
				params = append(params, map[string]any{"name": q.name, "in": "query", "required": q.required,
					"description": q.description, "schema": map[string]any{"type": q.typ}})
			}
			if rt.idempotent || rt.optionallyIdempotent() {
				params = append(params, map[string]any{"name": "Idempotency-Key", "in": "header", "required": rt.idempotent,
					"description": "Unique per logical operation; retries with the same key replay the original response.",
					"schema":      map[string]any{"type": "string", "maxLength": 255}})
			}
			if rt.access == cardNetwork {
				params = append(params, map[string]any{"name": "Zirnis-Signature", "in": "header", "required": true,
					"description": "t={unix},v1={hex HMAC-SHA256 of '{t}.{body}'}", "schema": map[string]any{"type": "string"}})
			}
			if params != nil {
				op["parameters"] = params
			}
			if rt.body != nil {
				op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": schemaOf(reflect.TypeOf(rt.body))}}}
			}
			switch rt.access {
			case public, cardNetwork:
				op["security"] = []any{}
			}
			if paths[rt.path] == nil {
				paths[rt.path] = map[string]any{}
			}
			paths[rt.path][strings.ToLower(rt.method)] = op
		}
		s.openapi = mustJSON(map[string]any{
			"openapi": "3.0.3",
			"info": map[string]any{
				"title":       "Zirnis 1.0 — Payments & Core Banking API",
				"version":     "1.0.0",
				"description": "Double-entry ledger, accounts, payments, payouts, FX, cards, disputes, reconciliation, GL and T24-style Close of Business.",
			},
			"servers": []any{map[string]any{"url": "/"}},
			"paths":   paths,
			"components": map[string]any{"securitySchemes": map[string]any{
				"ApiKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-Api-Key"},
				"Bearer": map[string]any{"type": "http", "scheme": "bearer"},
			}},
			"security": []any{map[string]any{"ApiKey": []string{}}, map[string]any{"Bearer": []string{}}},
		})
	})
	return s.openapi
}

func responses(rt route) map[string]any {
	out := map[string]any{strconv.Itoa(rt.status): map[string]any{"description": http.StatusText(rt.status)}}
	problem := map[string]any{"description": "Problem details", "content": map[string]any{"application/problem+json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	for _, code := range []string{"400", "404", "409", "422"} {
		out[code] = problem
	}
	if rt.access != public && rt.access != cardNetwork {
		out["401"], out["403"] = problem, problem
	}
	return out
}

func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '-' || r == '{' || r == '}' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

var (
	timeType    = reflect.TypeFor[time.Time]()
	dateType    = reflect.TypeFor[domain.Date]()
	uuidType    = reflect.TypeFor[uuid.UUID]()
	decimalType = reflect.TypeFor[decimal.Decimal]()
)

func schemaOf(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case dateType:
		return map[string]any{"type": "string", "format": "date"}
	case uuidType:
		return map[string]any{"type": "string", "format": "uuid"}
	case decimalType:
		return map[string]any{"type": "number"}
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaOf(t.Elem())}
	case reflect.Struct:
		props, required := map[string]any{}, []string{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			schema := schemaOf(f.Type)
			for _, rule := range strings.Split(f.Tag.Get("validate"), ",") {
				key, arg, _ := strings.Cut(rule, "=")
				n, _ := strconv.Atoi(arg)
				isString := schema["type"] == "string"
				switch key {
				case "required":
					required = append(required, name)
				case "min":
					schema[map[bool]string{true: "minLength", false: "minimum"}[isString]] = n
				case "max":
					schema[map[bool]string{true: "maxLength", false: "maximum"}[isString]] = n
				case "len":
					schema["minLength"], schema["maxLength"] = n, n
				case "email":
					schema["format"] = "email"
				case "url":
					schema["format"] = "uri"
				case "oneof":
					schema["enum"] = strings.Split(arg, "|")
				}
			}
			props[name] = schema
		}
		out := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			out["required"] = required
		}
		return out
	}
	return map[string]any{}
}

// docs serves an interactive API reference (Swagger UI) over the generated document.
func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Zirnis API</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
</head>
<body>
  <div id="docs"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-standalone-preset.js"></script>
  <script src="/docs/init.js"></script>
</body>
</html>`)
}
