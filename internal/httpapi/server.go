// Package httpapi is Zirnis's HTTP API: routing, authentication, idempotency, problem
// details, OpenAPI and the handlers for every module.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/config"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Server serves the API.
type Server struct {
	app    *app.App
	store  *store.Store
	cfg    config.Config
	logger *slog.Logger

	routes      []route
	openapiOnce sync.Once
	openapi     []byte
	// redis shares rate limits between instances (ZIRNIS_REDIS_URL); nil counts per instance.
	redis *redis.Client
}

type handler func(w http.ResponseWriter, r *http.Request) error

// route is one endpoint plus the metadata used for authorisation, idempotency and OpenAPI.
//
// Every state-changing route is idempotent: idempotent routes (money movement, records that
// can't be told apart) require an Idempotency-Key; every other POST/PUT/PATCH/DELETE honours one
// when sent. The only exceptions name the natural key that de-duplicates them instead
// (dedupedBy) — provider callbacks, which can't send our header.
type route struct {
	method, path string
	tag, summary string
	access       access
	idempotent   bool
	dedupedBy    string
	// noIdempotency: never stored for replay (sign-in and passwords: secrets in the request or
	// a session in the response).
	noIdempotency bool
	publicLimit   bool
	body          any     // request body type, for validation docs
	query         []param // documented query parameters
	status        int     // success status
	handle        handler
	// consoleAllowed: a money-moving route operators may use from the console (transactions,
	// whose console versions need a checker outside the sandbox).
	consoleAllowed bool
	// edge: also served on the payer edge (EdgeHandler), the one listener reachable from the
	// internet. Only public routes payers and providers need belong there.
	edge bool
	// analystMay: an analyst-only operator may call it too (analytics routes always may).
	analystMay bool
}

type param struct {
	name, typ, description string
	required               bool
}

func New(a *app.App, st *store.Store, cfg config.Config, logger *slog.Logger) *Server {
	s := &Server{app: a, store: st, cfg: cfg, logger: logger}
	if cfg.RedisURL != "" {
		if o, err := redis.ParseURL(cfg.RedisURL); err != nil {
			logger.Error("ZIRNIS_REDIS_URL is not a Redis URL; rate limits are counted per instance", "error", err)
		} else {
			s.redis = redis.NewClient(o)
		}
	}
	return s
}

func (s *Server) add(rt route) {
	if rt.status == 0 {
		rt.status = http.StatusOK
	}
	s.routes = append(s.routes, rt)
}

// Handler builds the HTTP handler (middleware order matters): every route, for the internal network.
func (s *Server) Handler() http.Handler {
	s.mapRoutes()
	r := s.router(s.recoverer, s.securityHeaders, s.correlation, s.cors, s.authenticate, s.rateLimit(s.newLimiter("api", s.cfg.RateLimitPerMinute)))
	s.mount(r, func(route) bool { return true })

	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/docs", http.StatusFound) })
	r.Get("/docs", s.docs)
	r.Get("/docs/init.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(`window.ui = SwaggerUIBundle({ url: '/openapi/v1.json', dom_id: '#docs', deepLinking: true,
  presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset], layout: 'BaseLayout' })`))
	})
	r.Get("/openapi/v1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.openAPI())
	})
	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) { s.health(w, http.StatusOK, "Healthy") })
	r.Get("/health/ready", s.ready)
	r.Method(http.MethodGet, "/ops/metrics", s.authorize(operatorRole, "", false, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.app.Metrics.Write(w)
	})))
	return r
}

// EdgeHandler is the payer edge: payment links, checkout returns and provider callbacks, and
// nothing else. It never reads an API key or a session, so the console and the API cannot be
// reached through it even if the proxy in front forwards more than it should.
func (s *Server) EdgeHandler() http.Handler {
	s.mapRoutes()
	r := s.router(s.recoverer, s.securityHeaders, s.correlation, s.edgeClient, s.rateLimit(s.newLimiter("edge", s.cfg.RateLimitPerMinute)))
	s.mount(r, func(rt route) bool {
		if rt.edge && rt.access != public {
			panic("httpapi: " + rt.method + " " + rt.path + " is on the edge but needs a caller")
		}
		return rt.edge
	})
	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) { s.health(w, http.StatusOK, "Healthy") })
	return r
}

func (s *Server) mapRoutes() {
	s.routes = nil
	s.mapAuth()
	s.mapCore()
	s.mapCustomers()
	s.mapAccounts()
	s.mapTransfers()
	s.mapTransactions()
	s.mapPayments()
	s.mapFx()
	s.mapCards()
	s.mapRisk()
	s.mapReconciliation()
	s.mapSuspense()
	s.mapCorrespondents()
	s.mapChannels()
	s.mapExtensions()
	s.mapLocalTables()
	s.mapEnquiries()
	s.mapGL()
	s.mapIntegrations()
	s.mapTrace()
	if s.cfg.SandboxEnabled() {
		s.mapSandbox()
	}
}

func (s *Server) router(middleware ...func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware...)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, newProblem(r, http.StatusNotFound, "not_found", "No resource matches this path."))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, newProblem(r, http.StatusMethodNotAllowed, "method_not_allowed", "This method is not allowed on this path."))
	})
	return r
}

// mount adds the routes a listener serves, each behind its idempotency, signature, access and
// rate-limit checks.
func (s *Server) mount(r chi.Router, serves func(route) bool) {
	public := s.newLimiter("public", s.cfg.PublicRatePerMinute)
	for _, rt := range s.routes {
		if !serves(rt) {
			continue
		}
		var h http.Handler = s.adapt(rt.handle)
		if rt.idempotent || rt.optionallyIdempotent() {
			h = s.idempotency(rt.idempotent, h)
		}
		if rt.access == cardNetwork {
			h = s.signed("cardnetwork", h)
		}
		h = s.authorize(rt.access, rt.permission(), rt.idempotent && rt.access == platformRole && !rt.consoleAllowed, rt.access == analystRole || rt.analystMay, h)
		if rt.publicLimit {
			h = s.rateLimit(public)(h)
		}
		r.Method(rt.method, rt.path, h)
	}
}

// optionallyIdempotent: a state-changing route without a natural de-duplication key honours an
// Idempotency-Key when the client sends one.
func (rt route) optionallyIdempotent() bool {
	return !rt.idempotent && rt.dedupedBy == "" && !rt.noIdempotency && rt.method != http.MethodGet && rt.method != http.MethodHead
}

// permission is the operator permission a route needs on top of its access: reading needs
// none, authorising (authorise, reject, approve) needs checker, any other change needs maker.
func (rt route) permission() string {
	switch {
	case rt.access == adminRole:
		return domain.PermAdmin
	case rt.access == analystRole:
		return domain.PermAnalyst
	case rt.access != operatorRole || rt.method == http.MethodGet || rt.method == http.MethodHead:
		return ""
	case strings.HasSuffix(rt.path, "/authorise") || strings.HasSuffix(rt.path, "/reject") || strings.HasSuffix(rt.path, "/approve"):
		return domain.PermChecker
	}
	return domain.PermMaker
}

func (s *Server) adapt(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			s.fail(w, r, err)
		}
	}
}

// health answers a probe. X-Zirnis-Sandbox lets a monitor alert on a sandbox that is meant to be
// production.
func (s *Server) health(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Zirnis-Sandbox", strconv.FormatBool(s.cfg.SandboxEnabled()))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

// ready is unhealthy without the database or while the process drains for shutdown, and degraded
// (but still 200) when a provider circuit is open — failover is active.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.app.Draining.Load() {
		s.health(w, http.StatusServiceUnavailable, "Draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.health(w, http.StatusServiceUnavailable, "Unhealthy")
		return
	}
	for _, p := range s.app.Ops.Providers() {
		if p.Circuit == string(app.CircuitOpen) {
			s.health(w, http.StatusOK, "Degraded")
			return
		}
	}
	s.health(w, http.StatusOK, "Healthy")
}

// ---------------------------------------------------------------------------------------------
// Request helpers

func pathID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, &apiError{status: http.StatusNotFound, code: "not_found", detail: "No resource matches this path."}
	}
	return id, nil
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, badRequest("bad_request", "Query parameter '%s' must be an integer.", name)
	}
	return n, nil
}

func queryDate(r *http.Request, name string) (*domain.Date, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	d, err := domain.ParseDate(v)
	if err != nil {
		return nil, badRequest("bad_request", "Query parameter '%s' must be a date (YYYY-MM-DD).", name)
	}
	return &d, nil
}

func queryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, badRequest("bad_request", "Query parameter '%s' must be a UUID.", name)
	}
	return &id, nil
}

// queryEnum parses an optional enum query parameter case-insensitively.
func queryEnum[T ~string](r *http.Request, name string, values ...T) (*T, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	parsed, ok := domain.ParseEnum(v, values...)
	if !ok {
		return nil, badRequest("bad_request", "Query parameter '%s' has an unknown value '%s'.", name, v)
	}
	return &parsed, nil
}

// enumOr parses a request enum field, falling back to a default when it is empty.
func enumOr[T ~string](field, value string, fallback T, values ...T) (T, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, ok := domain.ParseEnum(value, values...)
	if !ok {
		return fallback, &apiError{status: http.StatusBadRequest, code: "validation_failed", detail: "One or more validation errors occurred.",
			errors: map[string][]string{field: {"The " + field + " field has an unknown value '" + value + "'."}}}
	}
	return parsed, nil
}

func created(w http.ResponseWriter, location string, v any) error {
	w.Header().Set("Location", location)
	return writeJSON(w, http.StatusCreated, v)
}

func ok(w http.ResponseWriter, v any) error { return writeJSON(w, http.StatusOK, v) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
