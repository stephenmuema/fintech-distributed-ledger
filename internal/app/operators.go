package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Operators (EB.USER) are the people who sign in to the operations console. An operator record
// is maintained under maker-checker by administrators; the password is not part of it: an
// administrator resets it to a one-time password the operator must change at the next sign-in.
// What an operator may do is their permissions — maker (input), checker (authorise) and admin
// (maintain operators) — and four-eyes still applies: nobody authorises their own input.
type Operators struct{ a *App }

// BootstrapActor applies the first administrator's operator changes while no other
// administrator exists to authorise them.
const BootstrapActor = "system.bootstrap"

// sessionTouchEvery limits how often a session's last activity is written: every request
// reads the session, only one a minute writes it.
const sessionTouchEvery = time.Minute

func (o *Operators) List(ctx context.Context) ([]domain.Operator, error) {
	return store.Select[domain.Operator](o.a.read(ctx), "SELECT * FROM operators ORDER BY username")
}

func (o *Operators) Get(ctx context.Context, username string) (*domain.Operator, error) {
	return o.load(o.a.read(ctx), username)
}

func (o *Operators) load(q store.Q, username string) (*domain.Operator, error) {
	op, err := store.Get[domain.Operator](q, "SELECT * FROM operators WHERE username = ?", strings.ToLower(strings.TrimSpace(username)))
	if err == nil && op == nil {
		err = domain.NotFound("operator", username)
	}
	return op, err
}

// ---------------------------------------------------------------------------------------------
// Maintenance (maker-checker)

// ProposeOperator inputs a new operator or an amendment (INAU). Operators cannot maintain their
// own record, and the last active administrator cannot be removed. While no other administrator
// exists, the change is applied at once (the bootstrap administrator setting up the team).
func (p *Parameters) ProposeOperator(ctx context.Context, username string, spec domain.OperatorSpec) (*domain.ParameterChangeRequest, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if err := domain.ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	actor := store.Actor(ctx)
	if strings.EqualFold(actor, username) {
		return nil, domain.Rule("self_maintenance", "Operators cannot change their own record; ask another administrator.")
	}
	q := p.a.read(ctx)
	if err := p.a.Operators.checkAdmins(q, username, spec); err != nil {
		return nil, err
	}
	request, err := p.propose(ctx, domain.ParamOperator, username, spec)
	if err != nil {
		return nil, err
	}
	others, err := store.Scalar[int](q, "SELECT COUNT(*) FROM operators WHERE active = ? AND permissions LIKE ? AND username <> ?", true, "%"+domain.PermAdmin+"%", actor)
	if err != nil || others > 0 {
		return request, err
	}
	p.a.Logger.Warn("no other administrator can authorise operator changes; applying as bootstrap", "operator", username, "inputter", actor)
	return p.Authorise(store.WithActor(ctx, BootstrapActor), request.ID)
}

// checkAdmins refuses a change that would leave no active administrator.
func (o *Operators) checkAdmins(q store.Q, username string, spec domain.OperatorSpec) error {
	existing, err := store.Get[domain.Operator](q, "SELECT * FROM operators WHERE username = ?", username)
	if err != nil || existing == nil || !existing.Active || !existing.Can(domain.PermAdmin) {
		return err
	}
	stillAdmin := (spec.Active == nil || *spec.Active) && containsString(spec.Permissions, domain.PermAdmin)
	if stillAdmin {
		return nil
	}
	others, err := store.Scalar[int](q, "SELECT COUNT(*) FROM operators WHERE active = ? AND permissions LIKE ? AND username <> ?", true, "%"+domain.PermAdmin+"%", username)
	if err != nil {
		return err
	}
	if others == 0 {
		return domain.Rule("last_administrator", "%s is the last active administrator.", username)
	}
	return nil
}

func (o *Operators) apply(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.OperatorSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := spec.Normalize(); err != nil {
		return err
	}
	if err := o.checkAdmins(tx, request.Key, spec); err != nil {
		return err
	}
	op, err := store.Get[domain.Operator](tx, "SELECT * FROM operators WHERE username = ?", request.Key)
	if err != nil {
		return err
	}
	if op == nil {
		op = domain.NewOperator(request.Key, spec, tx.Now())
		if err := tx.Insert("operators", op); err != nil {
			return err
		}
		return tx.Audit("operator.created", "operator", op.Username, map[string]any{"permissions": op.Permissions})
	}
	op.Apply(spec)
	if err := tx.Update("operators", op); err != nil {
		return err
	}
	if !op.Active {
		return o.revokeSessions(tx, op.ID, "operator deactivated")
	}
	return nil
}

// ResetPassword sets a one-time password for another operator (administrators only, never for
// themselves), unlocks them and signs them out everywhere. The administrator chooses the
// password and hands it over; it is stored only as a hash and must be changed at the next
// sign-in.
func (o *Operators) ResetPassword(ctx context.Context, username, temporary string) error {
	return o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		op, err := o.load(tx, username)
		if err != nil {
			return err
		}
		if strings.EqualFold(tx.Actor(), op.Username) {
			return domain.Rule("self_maintenance", "Change your own password instead.")
		}
		if err := op.SetPassword(temporary, true, tx.Now()); err != nil {
			return err
		}
		if err := o.savePassword(tx, op); err != nil {
			return err
		}
		if err := o.revokeSessions(tx, op.ID, "password reset"); err != nil {
			return err
		}
		return tx.Audit("operator.password_reset", "operator", op.Username, nil)
	})
}

func (o *Operators) savePassword(tx *store.Tx, op *domain.Operator) error {
	before, err := store.Get[domain.Operator](tx, "SELECT * FROM operators WHERE id = ?", op.ID)
	if err != nil {
		return err
	}
	// Login bookkeeping and passwords are not part of the maintained record: no $HIS version.
	// The change log still shows that the password changed (never the hash).
	if _, err := tx.Exec("UPDATE operators SET password_hash = ?, must_change_password = ?, password_changed_at = ?, failed_logins = ?, locked_until = ? WHERE id = ?",
		op.PasswordHash, op.MustChangePassword, op.PasswordChangedAt, op.FailedLogins, op.LockedUntil, op.ID); err != nil {
		return err
	}
	if before == nil {
		return nil
	}
	return tx.LogChanges(op.HistoryName(), op.ID, op.Version, before, op)
}

func (o *Operators) revokeSessions(tx *store.Tx, operatorID uuid.UUID, reason string) error {
	if _, err := tx.Exec("UPDATE operator_sessions SET revoked_at = ? WHERE operator_id = ? AND revoked_at IS NULL", tx.Now(), operatorID); err != nil {
		return err
	}
	return tx.Audit("operator.sessions_revoked", "operator", operatorID.String(), map[string]any{"reason": reason})
}

// ---------------------------------------------------------------------------------------------
// Sign-in

// ErrInvalidCredentials is the one answer to a wrong username or password.
var ErrInvalidCredentials = domain.Rule("invalid_credentials", "The username or password is incorrect.")

// Login checks an operator's password and opens a session. It returns the session token,
// which only the caller ever sees. Wrong passwords count towards a temporary lock-out.
func (o *Operators) Login(ctx context.Context, username, password, remoteAddr, userAgent string) (string, *domain.Operator, error) {
	op, err := store.Get[domain.Operator](o.a.read(ctx), "SELECT * FROM operators WHERE username = ?", strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return "", nil, err
	}
	if op == nil {
		domain.VerifyDummyPassword(password) // same work as a real check: no username probing by timing
		return "", nil, ErrInvalidCredentials
	}
	ctx = store.WithActor(ctx, op.Username)
	now := o.a.now()
	if err := op.CanLogIn(now); err != nil {
		return "", nil, err
	}
	if !domain.VerifyPassword(password, op.PasswordHash) {
		err := o.a.db.Atomic(ctx, func(tx *store.Tx) error {
			current, err := o.load(tx, op.Username)
			if err != nil {
				return err
			}
			current.RecordFailedLogin(tx.Now())
			if _, err := tx.Exec("UPDATE operators SET failed_logins = ?, locked_until = ? WHERE id = ?", current.FailedLogins, current.LockedUntil, current.ID); err != nil {
				return err
			}
			return tx.Audit("operator.login_failed", "operator", op.Username, map[string]any{"remoteAddr": remoteAddr, "locked": current.LockedUntil != nil})
		})
		if err != nil {
			return "", nil, err
		}
		return "", nil, ErrInvalidCredentials
	}
	var token string
	err = o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		op.RecordLogin(tx.Now())
		if _, err := tx.Exec("UPDATE operators SET failed_logins = ?, locked_until = ?, last_login_at = ? WHERE id = ?", op.FailedLogins, op.LockedUntil, op.LastLoginAt, op.ID); err != nil {
			return err
		}
		var session *domain.OperatorSession
		token, session = domain.NewSessionToken(op.ID, remoteAddr, userAgent, tx.Now())
		if err := tx.Insert("operator_sessions", session); err != nil {
			return err
		}
		return tx.Audit("operator.login", "operator", op.Username, map[string]any{"remoteAddr": remoteAddr, "session": session.ID})
	})
	return token, op, err
}

// Authenticate resolves a session token to its operator, or nil when the session is unknown,
// expired, idle too long or revoked, or the operator is no longer active.
func (o *Operators) Authenticate(ctx context.Context, token string) (*domain.Operator, error) {
	q := o.a.read(ctx)
	session, err := store.Get[domain.OperatorSession](q, "SELECT * FROM operator_sessions WHERE token_hash = ?", domain.TokenHash(token))
	now := o.a.now()
	if err != nil || session == nil || !session.Live(now) {
		return nil, err
	}
	op, err := store.Get[domain.Operator](q, "SELECT * FROM operators WHERE id = ?", session.OperatorID)
	if err != nil || op == nil || !op.Active {
		return nil, err
	}
	if now.Sub(session.LastSeenAt) >= sessionTouchEvery {
		if err := o.a.db.Atomic(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec("UPDATE operator_sessions SET last_seen_at = ? WHERE id = ?", now, session.ID)
			return err
		}); err != nil {
			return nil, err
		}
	}
	return op, nil
}

func (o *Operators) Logout(ctx context.Context, token string) error {
	return o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE operator_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL", tx.Now(), domain.TokenHash(token))
		if err != nil {
			return err
		}
		return tx.Audit("operator.logout", "operator", tx.Actor(), nil)
	})
}

// ChangePassword sets an operator's own password (their current one is required) and signs out
// their other sessions.
func (o *Operators) ChangePassword(ctx context.Context, username, token, current, next string) error {
	return o.a.db.Atomic(ctx, func(tx *store.Tx) error {
		op, err := o.load(tx, username)
		if err != nil {
			return err
		}
		if !domain.VerifyPassword(current, op.PasswordHash) {
			return domain.Rule("invalid_credentials", "The current password is incorrect.")
		}
		if current == next {
			return domain.Rule("weak_password", "Choose a password different from the current one.")
		}
		if err := op.SetPassword(next, false, tx.Now()); err != nil {
			return err
		}
		if err := o.savePassword(tx, op); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE operator_sessions SET revoked_at = ? WHERE operator_id = ? AND token_hash <> ? AND revoked_at IS NULL",
			tx.Now(), op.ID, domain.TokenHash(token)); err != nil {
			return err
		}
		return tx.Audit("operator.password_changed", "operator", op.Username, nil)
	})
}

// ---------------------------------------------------------------------------------------------
// Bootstrap

// demoOperators are the sandbox console's operators. Their password is public (SECURITY.md).
var demoOperators = []struct {
	username string
	spec     domain.OperatorSpec
}{
	{"amina", domain.OperatorSpec{Name: "Amina Otieno", Email: "amina@zirnis.example", Permissions: []string{domain.PermMaker}}},
	{"brian", domain.OperatorSpec{Name: "Brian Kamau", Email: "brian@zirnis.example", Permissions: []string{domain.PermChecker, domain.PermAdmin}}},
	{"admin", domain.OperatorSpec{Name: "Console administrator", Email: "admin@zirnis.example", Permissions: []string{domain.PermAdmin}}},
	{"wanjiru", domain.OperatorSpec{Name: "Wanjiru Mwangi", Email: "wanjiru@zirnis.example", Permissions: []string{domain.PermAnalyst}}},
}

// DemoOperatorPassword is the sandbox operators' public password.
const DemoOperatorPassword = "zirnis-sandbox-demo"

// ensureOperators creates the first operators: in the sandbox, each demo operator that does not
// exist yet (so a sandbox seeded before a new demo operator gets them too); otherwise, when there
// are none, one administrator whose password must be changed at the first sign-in.
func (o *Operators) ensureOperators(tx *store.Tx) error {
	if o.a.Options.DemoOperators {
		var created []string
		for _, d := range demoOperators {
			exists, err := store.Exists(tx, "SELECT 1 FROM operators WHERE username = ?", d.username)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			spec := d.spec
			spec.Permissions = slices.Clone(spec.Permissions) // Normalize rewrites them in place; the demo list is shared
			if err := spec.Normalize(); err != nil {
				return err
			}
			op := domain.NewOperator(d.username, spec, tx.Now())
			if err := op.SetPassword(DemoOperatorPassword, false, tx.Now()); err != nil {
				return err
			}
			if err := tx.Insert("operators", op); err != nil {
				return err
			}
			created = append(created, d.username)
		}
		if len(created) == 0 {
			return nil
		}
		o.a.Logger.Info("sandbox console operators created", "operators", created,
			"permissions", "amina (maker), brian (checker, admin), admin (admin), wanjiru (analyst)")
		return tx.Audit("operator.demo_seeded", "operator", "sandbox", map[string]any{"created": created})
	}
	exists, err := store.Exists(tx, "SELECT 1 FROM operators")
	if err != nil || exists {
		return err
	}
	username := o.a.Options.BootstrapAdmin
	if username == "" {
		username = "admin"
	}
	if err := domain.ValidateUsername(username); err != nil {
		return err
	}
	spec := domain.OperatorSpec{Name: "Bootstrap administrator", Email: username + "@localhost.invalid", Permissions: []string{domain.PermAdmin}}
	if err := spec.Normalize(); err != nil {
		return err
	}
	op := domain.NewOperator(username, spec, tx.Now())
	password, generated := o.a.Options.BootstrapAdminPassword, false
	if password == "" {
		password, generated = domain.TemporaryPassword(), true
	}
	if err := op.SetPassword(password, true, tx.Now()); err != nil {
		return err
	}
	if err := tx.Insert("operators", op); err != nil {
		return err
	}
	if generated {
		o.a.Logger.Warn("first administrator created with a one-time password: sign in to the console and change it",
			"username", username, "password", password)
	}
	return tx.Audit("operator.bootstrap_admin", "operator", username, nil)
}
