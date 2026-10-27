package httpapi

import (
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/stephenmuema/finnie/internal/domain"
)

const tagAuth = "EB · Operators & sign-in"

type loginRequest struct {
	Username string `json:"username" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"required"`
	NewPassword     string `json:"newPassword" validate:"required"`
}

type resetPasswordRequest struct {
	TemporaryPassword string `json:"temporaryPassword" validate:"required"`
}

// sessionView is who the caller is, as the console shows it.
type sessionView struct {
	Username           string   `json:"username"`
	Name               string   `json:"name"`
	Permissions        []string `json:"permissions"`
	MustChangePassword bool     `json:"mustChangePassword"`
	// CSRFToken must accompany every state-changing request made with the session cookie.
	CSRFToken string `json:"csrfToken,omitempty"`
	// Sandbox: the console may play the platform (demo money movement); never in production.
	Sandbox bool `json:"sandbox"`
}

func (s *Server) viewOf(p *principal) sessionView {
	v := sessionView{Username: p.Name, Name: p.DisplayName, Permissions: []string{}, MustChangePassword: p.MustChangePassword, Sandbox: s.cfg.SandboxEnabled()}
	for _, perm := range domain.Permissions {
		if p.can(perm) {
			v.Permissions = append(v.Permissions, perm)
		}
	}
	if v.Name == "" {
		v.Name = p.Name
	}
	if p.Session != "" {
		v.CSRFToken = domain.CSRFToken(p.Session)
	}
	return v
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")})
}

func remoteAddr(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) mapAuth() {
	s.add(route{method: "POST", path: "/auth/login", tag: tagAuth, access: public, publicLimit: true, noIdempotency: true, body: loginRequest{},
		summary: "Sign in to the console: sets an HttpOnly session cookie and returns the session's CSRF token",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req loginRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			token, op, err := s.app.Operators.Login(r.Context(), req.Username, req.Password, remoteAddr(r), r.UserAgent())
			if derr, ok := domain.AsError(err); ok && derr.Code == "invalid_credentials" {
				return &apiError{status: http.StatusUnauthorized, code: derr.Code, detail: derr.Message}
			}
			if err != nil {
				return err
			}
			s.setSessionCookie(w, r, token, int(domain.SessionMax.Seconds()))
			return ok(w, s.viewOf(&principal{Name: op.Username, DisplayName: op.Name, Session: token, MustChangePassword: op.MustChangePassword,
				Roles: append([]string{"platform", "operator"}, op.Permissions...)}))
		}})
	s.add(route{method: "POST", path: "/auth/logout", tag: tagAuth, access: signedIn, noIdempotency: true, summary: "Sign out of the console",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			if p := principalOf(r); p.Session != "" {
				if err := s.app.Operators.Logout(r.Context(), p.Session); err != nil {
					return err
				}
			}
			s.setSessionCookie(w, r, "", -1)
			w.WriteHeader(http.StatusNoContent)
			return nil
		}})
	s.add(route{method: "GET", path: "/auth/session", tag: tagAuth, access: signedIn, summary: "Who is signed in, their permissions and CSRF token",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return ok(w, s.viewOf(principalOf(r)))
		}})
	s.add(route{method: "POST", path: "/auth/password", tag: tagAuth, access: signedIn, noIdempotency: true, body: changePasswordRequest{},
		summary: "Change your own password (signs out your other sessions)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			p := principalOf(r)
			if p.Session == "" {
				return &apiError{status: http.StatusForbidden, code: "session_required", detail: "API clients have no password."}
			}
			var req changePasswordRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			if err := s.app.Operators.ChangePassword(r.Context(), p.Name, p.Session, req.CurrentPassword, req.NewPassword); err != nil {
				return err
			}
			p.MustChangePassword = false
			return ok(w, s.viewOf(p))
		}})

	s.add(route{method: "GET", path: "/ops/operators", tag: tagAuth, access: adminRole, summary: "Console operators",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Operators.List(r.Context()))
		}})
	s.add(route{method: "GET", path: "/ops/operators/{username}", tag: tagAuth, access: adminRole, summary: "An operator",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			return respond(w)(s.app.Operators.Get(r.Context(), chi.URLParam(r, "username")))
		}})
	s.add(route{method: "POST", path: "/ops/operators/{username}", tag: tagAuth, access: adminRole, status: http.StatusAccepted, body: domain.OperatorSpec{},
		summary: "Input a new operator or an amendment (INAU: another administrator authorises it)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var spec domain.OperatorSpec
			if err := decode(r, &spec); err != nil {
				return err
			}
			change, err := s.app.Parameters.ProposeOperator(r.Context(), chi.URLParam(r, "username"), spec)
			return accepted(w, "/ops/parameters/pending", change, err)
		}})
	s.add(route{method: "POST", path: "/ops/operators/{username}/password", tag: tagAuth, access: adminRole, noIdempotency: true, body: resetPasswordRequest{},
		summary: "Set a one-time password for another operator (they must change it at the next sign-in)",
		handle: func(w http.ResponseWriter, r *http.Request) error {
			var req resetPasswordRequest
			if err := decode(r, &req); err != nil {
				return err
			}
			username := chi.URLParam(r, "username")
			if err := s.app.Operators.ResetPassword(r.Context(), username, req.TemporaryPassword); err != nil {
				return err
			}
			return respond(w)(s.app.Operators.Get(r.Context(), username))
		}})
}

// requireAdminFor refuses to let a non-administrator decide an operator change.
func (s *Server) requireAdminFor(r *http.Request, id uuidT) error {
	request, err := s.app.Parameters.Request(r.Context(), id)
	if err != nil {
		return err
	}
	if request.Type == domain.ParamOperator && !principalOf(r).can(domain.PermAdmin) {
		return &apiError{status: http.StatusForbidden, code: "permission_required", detail: "Operator changes are decided by an administrator."}
	}
	return nil
}
