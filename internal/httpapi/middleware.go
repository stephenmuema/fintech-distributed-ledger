package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/webhook"
)

type ctxKey int

const (
	principalKey ctxKey = iota
	correlationKey
	edgeClientKey
)

// principal is the authenticated caller: an API client, or an operator signed in to the console.
type principal struct {
	Name    string
	Roles   []string // access roles and, for operators, permissions (maker, checker, admin)
	Invalid bool     // a key or session was presented but is not valid
	// Session is the console session token when the operator signed in (cookie), "" for API keys.
	Session            string
	DisplayName        string
	MustChangePassword bool
}

// can reports whether the caller holds an operator permission. Operator API keys configured
// without any permission predate them and may input and authorise.
// analystOnly reports an operator whose only permission is analyst.
func (p *principal) analystOnly() bool {
	return slices.Contains(p.Roles, domain.PermAnalyst) &&
		!slices.ContainsFunc(p.Roles, func(r string) bool { return r != domain.PermAnalyst && slices.Contains(domain.Permissions, r) })
}

func (p *principal) can(permission string) bool {
	if slices.Contains(p.Roles, permission) {
		return true
	}
	legacyKey := p.Session == "" && slices.Contains(p.Roles, "operator") &&
		!slices.ContainsFunc(p.Roles, func(r string) bool { return slices.Contains(domain.Permissions, r) })
	return legacyKey && permission != domain.PermAdmin
}

// sessionCookie carries a console session: HttpOnly, SameSite=Strict, and every state-changing
// request made with it must also carry the session's CSRF token (X-CSRF-Token), which only our
// own pages can read.
const sessionCookie = "zirnis_session"

func principalOf(r *http.Request) *principal {
	p, _ := r.Context().Value(principalKey).(*principal)
	return p
}

func correlationID(r *http.Request) string {
	id, _ := r.Context().Value(correlationKey).(string)
	return id
}

// recoverer turns a panic into a 500 problem instead of a dropped connection.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				logRecovered(s.logger, r, p)
				writeProblem(w, newProblem(r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred."))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets defensive headers for a JSON API that returns financial data: nothing may
// be cached, sniffed, framed or used as a script/style source. The interactive API reference
// (/docs) is exempt from the strict CSP because it loads its own UI assets.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if r.URL.Path == "/docs" || strings.HasPrefix(r.URL.Path, "/docs/") {
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self' https://cdn.jsdelivr.net; style-src https://cdn.jsdelivr.net 'unsafe-inline'; "+
				"img-src 'self' data: https://cdn.jsdelivr.net; connect-src 'self'; frame-ancestors 'none'")
		} else {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			h.Set("Pragma", "no-cache")
		}
		if !s.cfg.IsDevelopment() && (r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")) {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// correlation propagates or issues X-Correlation-Id and records request latency.
func (s *Server) correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-Id")
		if id == "" || len(id) > 64 {
			id = domain.RandomHex(16)
		}
		w.Header().Set("X-Correlation-Id", id)
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		ctx := store.WithCorrelation(context.WithValue(r.Context(), correlationKey, id), id)
		next.ServeHTTP(rec, r.WithContext(ctx))

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		s.app.Metrics.HTTPRequests.Observe(float64(time.Since(started).Milliseconds()), "method", r.Method, "route", route, "status", strconv.Itoa(rec.status))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// cors allows the configured browser origins (the React console) to call the API.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.cfg.CorsOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Idempotent-Replayed, X-Correlation-Id")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", r.Header.Get("Access-Control-Request-Method"))
				h.Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves "Authorization: Bearer <key>" or "X-Api-Key" against SHA-256 key
// hashes, compared in constant time, or else the console session cookie. It never rejects an
// unauthenticated request (authorize does), but it refuses a cookie-authenticated change that
// lacks the session's CSRF token.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get("X-Api-Key")
		if auth := r.Header.Get("Authorization"); presented == "" && len(auth) > 7 && strings.EqualFold(auth[:7], "Bearer ") {
			presented = strings.TrimSpace(auth[7:])
		}
		if presented == "" {
			cookie, err := r.Cookie(sessionCookie)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			p, err := s.sessionPrincipal(r.Context(), cookie.Value)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			changes := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
			if !p.Invalid && changes && r.URL.Path != "/auth/login" &&
				subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(domain.CSRFToken(cookie.Value))) != 1 {
				writeProblem(w, newProblem(r, http.StatusForbidden, "csrf_failed", "This request did not come from the console; reload it and try again."))
				return
			}
			ctx := context.WithValue(r.Context(), principalKey, p)
			if !p.Invalid {
				ctx = store.WithActor(ctx, p.Name)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		hash := sha256.Sum256([]byte(presented))
		p := &principal{Invalid: true}
		for _, client := range s.cfg.APIKeys {
			expected, err := hex.DecodeString(client.KeyHash)
			if err == nil && len(expected) == len(hash) && subtle.ConstantTimeCompare(hash[:], expected) == 1 {
				if !fromAllowedNetwork(r, client.AllowedCIDRs) {
					s.logger.Warn("API key used from outside its allowed networks", "client", client.Name, "remote", r.RemoteAddr)
					s.auditFailure(r, http.StatusUnauthorized, fmt.Errorf("api key %s used from %s, outside its allowed networks", client.Name, r.RemoteAddr))
					break
				}
				p = &principal{Name: client.Name, Roles: client.Roles}
			}
		}
		ctx := context.WithValue(r.Context(), principalKey, p)
		if !p.Invalid {
			actor, err := onBehalfOf(p.Name, r.Header.Get("On-Behalf-Of"))
			if err != nil {
				s.fail(w, r, err)
				return
			}
			ctx = store.WithActor(ctx, actor)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

var subjectPattern = regexp.MustCompile(`^[A-Za-z0-9._:@-]{1,64}$`)

// onBehalfOf is who an API client acts for: the app's backend names the customer or staff member
// behind a request (On-Behalf-Of: cust-8841), and every record it writes, the change log and the
// audit trail show "platform/cust-8841". Only API clients may name one.
func onBehalfOf(client, subject string) (string, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return client, nil
	}
	if !subjectPattern.MatchString(subject) {
		return "", badRequest("invalid_on_behalf_of", "On-Behalf-Of must be 1–64 letters, digits or . _ : @ -")
	}
	return client + "/" + subject, nil
}

// fromAllowedNetwork checks the caller's address (the TCP peer: a key with an allowlist is used
// directly, or the proxy in front is what the allowlist names) against a key's networks.
func fromAllowedNetwork(r *http.Request, cidrs []string) bool {
	if len(cidrs) == 0 {
		return true
	}
	return inNetworks(net.ParseIP(peer(r)), cidrs)
}

func inNetworks(ip net.IP, cidrs []string) bool {
	for _, c := range cidrs {
		if _, network, err := net.ParseCIDR(c); err == nil && ip != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

// peer is the TCP peer's address.
func peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// edgeClient names the payer behind the edge's proxy, so public rate limits apply per payer and
// not to the proxy's one address. Only a trusted proxy's X-Real-IP is believed.
func (s *Server) edgeClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := peer(r)
		if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil && inNetworks(net.ParseIP(client), s.cfg.EdgeTrustedProxies) {
			client = real.String()
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), edgeClientKey, client)))
	})
}

// fixedWindow is a per-key fixed-window rate limiter.
type fixedWindow struct {
	limit  int
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func newFixedWindow(limit int) *fixedWindow {
	return &fixedWindow{limit: limit, counts: map[string]int{}}
}

func (f *fixedWindow) allow(key string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if start := now.Truncate(time.Minute); !start.Equal(f.window) {
		f.window, f.counts = start, map[string]int{}
	}
	f.counts[key]++
	return f.counts[key] <= f.limit
}

func clientKey(r *http.Request) string {
	if p := principalOf(r); p != nil && !p.Invalid {
		return p.Name
	}
	if client, ok := r.Context().Value(edgeClientKey).(string); ok {
		return client
	}
	return peer(r)
}

func (s *Server) rateLimit(limiter limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.allow(clientKey(r), time.Now()) {
				w.Header().Set("Retry-After", "60")
				writeProblem(w, newProblem(r, http.StatusTooManyRequests, "rate_limited", "Too many requests; slow down and retry."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// sessionPrincipal resolves a console session cookie (Invalid when it is unknown or expired).
func (s *Server) sessionPrincipal(ctx context.Context, token string) (*principal, error) {
	op, err := s.app.Operators.Authenticate(ctx, token)
	if err != nil || op == nil {
		return &principal{Invalid: true}, err
	}
	return &principal{Name: op.Username, DisplayName: op.Name, Session: token, MustChangePassword: op.MustChangePassword,
		Roles: append([]string{"platform", "operator"}, op.Permissions...)}, nil
}

// access is who may call a route.
type access int

const (
	public access = iota
	platformRole
	operatorRole
	cardNetwork // HMAC-signed by the card network
	adminRole   // operators with the admin permission
	signedIn    // any operator, even one who must change their password first
	analystRole // operators with the analyst permission (business intelligence)
)

// authorize checks the caller's access role and, for operators, the permission the route
// needs (route.permission). movesMoney marks the product API's money-moving routes.
//
// An analyst-only operator (BI) reaches only the routes analystMay marks, and their own session.
func (s *Server) authorize(a access, permission string, movesMoney, analystMay bool, next http.Handler) http.Handler {
	role := map[access]string{platformRole: "platform", operatorRole: "operator", adminRole: "operator", signedIn: "operator", analystRole: "operator"}[a]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if role != "" {
			p := principalOf(r)
			if p == nil || p.Invalid {
				w.Header().Set("WWW-Authenticate", `Bearer realm="zirnis"`)
				writeProblem(w, newProblem(r, http.StatusUnauthorized, "unauthorized", "Sign in, or present a valid API key."))
				return
			}
			if !slices.Contains(p.Roles, role) {
				writeProblem(w, newProblem(r, http.StatusForbidden, "forbidden", "This API client is not allowed to perform this operation."))
				return
			}
			if p.analystOnly() && !analystMay && a != signedIn {
				writeProblem(w, newProblem(r, http.StatusForbidden, "analyst_only",
					"Business intelligence users see analytics only; ask an administrator for another permission."))
				return
			}
			// Customer money moves through the product API under the platform's own controls;
			// an operator signed in to the console may look, not move it (except in the sandbox,
			// whose demo console plays the platform).
			if movesMoney && p.Session != "" && !s.cfg.SandboxEnabled() {
				writeProblem(w, newProblem(r, http.StatusForbidden, "api_key_required",
					"Operators cannot move customer money from the console; it moves through the platform's API, or through maker-checker operations."))
				return
			}
			if p.MustChangePassword && a != signedIn {
				writeProblem(w, newProblem(r, http.StatusForbidden, "password_change_required", "Change your one-time password before continuing."))
				return
			}
			if permission != "" && !p.can(permission) {
				writeProblem(w, newProblem(r, http.StatusForbidden, "permission_required",
					"This needs the "+permission+" permission; ask an administrator."))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// signed verifies an HMAC-signed request (Zirnis-Signature: t=..,v1=..) over the raw body before
// the handler runs. Used for the card-network interface.
func (s *Server) signed(partner string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			s.fail(w, r, badRequest("bad_request", "Could not read the request body."))
			return
		}
		secret, ok := s.cfg.WebhookSecrets[partner]
		if !ok || !webhook.Verify(r.Header.Get("Zirnis-Signature"), secret, string(body), s.app.Clock.Now()) {
			writeProblem(w, newProblem(r, http.StatusUnauthorized, "invalid_signature", "Invalid request signature."))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// idempotency is API-level idempotency (Stripe-style) for every state-changing endpoint:
//   - the Idempotency-Key header is scoped to the API client; it is mandatory on money-moving
//     endpoints (required) and honoured, when sent, everywhere else;
//   - a unique index on (client, key) is the lock — a concurrent duplicate gets 409;
//   - the first response (any status below 500) is stored and replayed byte for byte;
//   - reusing a key with a different request (method, path or body) is rejected with 422;
//   - 5xx responses release the key so the client can safely retry.
//
// Records are kept 24 hours. They are written in their own transactions, so the business unit
// of work can never roll back (or accidentally commit) the idempotency record.
func (s *Server) idempotency(required bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" && !required {
			next.ServeHTTP(w, r)
			return
		}
		if strings.TrimSpace(key) == "" || len(key) > 255 {
			writeProblem(w, newProblem(r, http.StatusBadRequest, "idempotency_key_required",
				"This request must carry an 'Idempotency-Key' header (max 255 chars)."))
			return
		}
		client := "public"
		if p := principalOf(r); p != nil && !p.Invalid {
			client = p.Name
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			s.fail(w, r, badRequest("bad_request", "Could not read the request body."))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256([]byte(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery + "\n" + string(body)))
		fingerprint := hex.EncodeToString(sum[:])

		var existing *domain.IdempotencyRecord
		ctx := context.WithoutCancel(r.Context())
		err = s.store.Atomic(ctx, func(tx *store.Tx) (err error) {
			if existing, err = store.Get[domain.IdempotencyRecord](tx, "SELECT * FROM idempotency_records WHERE client_id = ? AND idempotency_key = ?", client, key); err != nil || existing != nil {
				return err
			}
			return tx.Insert("idempotency_records", &domain.IdempotencyRecord{ID: domain.NewID(), ClientID: client, Key: key,
				RequestHash: fingerprint, State: domain.IdempotencyInProgress, CreatedAt: tx.Now()})
		})
		switch {
		case store.IsUniqueViolation(err):
			writeProblem(w, newProblem(r, http.StatusConflict, "request_in_progress", "A request with this idempotency key is already being processed."))
			return
		case err != nil:
			s.fail(w, r, err)
			return
		case existing != nil:
			s.replay(w, r, existing, fingerprint)
			return
		}

		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		func() {
			defer func() {
				if p := recover(); p != nil {
					logRecovered(s.logger, r, p)
					rec.reset()
					writeProblem(rec, newProblem(r, http.StatusInternalServerError, "internal_error", "An unexpected error occurred."))
				}
			}()
			next.ServeHTTP(rec, r)
		}()

		err = s.store.Atomic(ctx, func(tx *store.Tx) error {
			if rec.status >= 500 { // let the client retry
				_, err := tx.Exec("DELETE FROM idempotency_records WHERE client_id = ? AND idempotency_key = ?", client, key)
				return err
			}
			_, err := tx.Exec("UPDATE idempotency_records SET state = ?, response_status = ?, response_body = ?, response_content_type = ? WHERE client_id = ? AND idempotency_key = ?",
				domain.IdempotencyCompleted, rec.status, rec.body.String(), rec.header.Get("Content-Type"), client, key)
			return err
		})
		if err != nil {
			s.logger.Error("failed to persist idempotent response", "key", key, "error", err)
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request, record *domain.IdempotencyRecord, fingerprint string) {
	switch {
	case record.RequestHash != fingerprint:
		writeProblem(w, newProblem(r, http.StatusUnprocessableEntity, "idempotency_key_reused", "This idempotency key was already used with a different request."))
	case record.State == domain.IdempotencyInProgress:
		writeProblem(w, newProblem(r, http.StatusConflict, "request_in_progress", "A request with this idempotency key is already being processed."))
	default:
		if record.ResponseContentType != nil {
			w.Header().Set("Content-Type", *record.ResponseContentType)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(*record.ResponseStatus)
		if record.ResponseBody != nil {
			_, _ = io.WriteString(w, *record.ResponseBody)
		}
	}
}

// bufferedResponse captures a handler's response so it can be stored before it is sent.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if !b.wrote {
		b.status, b.wrote = code, true
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}

func (b *bufferedResponse) reset() {
	b.header, b.status, b.wrote = http.Header{}, http.StatusOK, false
	b.body.Reset()
}
