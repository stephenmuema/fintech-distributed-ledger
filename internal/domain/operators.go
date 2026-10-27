package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"
)

// ---------------------------------------------------------------------------------------------
// Operators (T24: USER with its SMS profile)

// Operator permissions. Every operator may enquire; what they may change depends on these.
const (
	PermMaker   = "maker"   // input records and changes
	PermChecker = "checker" // authorise or reject another operator's input
	PermAdmin   = "admin"   // maintain operators and reset their passwords
	// PermAnalyst opens business intelligence. An operator with no other permission sees only
	// that: aggregates and charts, never customer records, queues or inputs.
	PermAnalyst = "analyst"
)

var Permissions = []string{PermMaker, PermChecker, PermAdmin, PermAnalyst}

// Login protection.
const (
	MaxFailedLogins   = 5
	LockoutDuration   = 15 * time.Minute
	MinPasswordLength = 12
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

// Operator is a person who signs in to the operations console. Operators are maintained under
// maker-checker like any parameter (versioned in $HIS); their password never is: it is set by
// the operator, or reset by an administrator to a one-time password the operator must change.
type Operator struct {
	ID          uuid.UUID  `db:"id" json:"id"`
	Version     int64      `db:"version" json:"version"`
	Username    string     `db:"username" json:"username"`
	Name        string     `db:"name" json:"name"`
	Email       string     `db:"email" json:"email"`
	Permissions StringList `db:"permissions" json:"permissions"`
	Active      bool       `db:"active" json:"active"`
	// PasswordHash is an Argon2id hash in PHC format ("" until a password is issued).
	PasswordHash       string     `db:"password_hash" json:"-"`
	MustChangePassword bool       `db:"must_change_password" json:"mustChangePassword"`
	PasswordChangedAt  *time.Time `db:"password_changed_at" json:"passwordChangedAt"`
	FailedLogins       int        `db:"failed_logins" json:"failedLogins"`
	LockedUntil        *time.Time `db:"locked_until" json:"lockedUntil"`
	LastLoginAt        *time.Time `db:"last_login_at" json:"lastLoginAt"`
	CreatedAt          time.Time  `db:"created_at" json:"createdAt"`
}

func (o *Operator) HistoryName() string { return "Operator" }

func (o *Operator) Can(permission string) bool { return slices.Contains(o.Permissions, permission) }

// OperatorSpec is the maintainable part of an operator (no password).
type OperatorSpec struct {
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Permissions []string `json:"permissions"`
	Active      *bool    `json:"active,omitempty"`
}

func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return Rule("invalid_username", "A username is 3–32 lower-case letters, digits, dots, dashes or underscores, starting with a letter.")
	}
	return nil
}

func (s *OperatorSpec) Normalize() error {
	s.Name, s.Email = strings.TrimSpace(s.Name), strings.TrimSpace(strings.ToLower(s.Email))
	if s.Name == "" {
		return Rule("invalid_operator", "A name is required.")
	}
	if _, err := mail.ParseAddress(s.Email); err != nil {
		return Rule("invalid_operator", "A valid email address is required.")
	}
	if len(s.Permissions) == 0 {
		return Rule("invalid_operator", "An operator needs at least one permission (%s).", strings.Join(Permissions, ", "))
	}
	for i, p := range s.Permissions {
		p = strings.ToLower(strings.TrimSpace(p))
		if !slices.Contains(Permissions, p) {
			return Rule("invalid_operator", "Unknown permission '%s' (%s).", p, strings.Join(Permissions, ", "))
		}
		s.Permissions[i] = p
	}
	s.Permissions = uniqueSorted(s.Permissions)
	return nil
}

func NewOperator(username string, spec OperatorSpec, now time.Time) *Operator {
	o := &Operator{ID: NewID(), Username: username, Active: true, MustChangePassword: true, CreatedAt: now}
	o.Apply(spec)
	return o
}

func (o *Operator) Apply(spec OperatorSpec) {
	o.Name, o.Email, o.Permissions = spec.Name, spec.Email, StringList(spec.Permissions)
	if spec.Active != nil {
		o.Active = *spec.Active
	}
}

// CanLogIn reports why an operator may not sign in now (nil when they may).
func (o *Operator) CanLogIn(now time.Time) error {
	switch {
	case !o.Active:
		return Rule("operator_inactive", "This operator is not active.")
	case o.LockedUntil != nil && now.Before(*o.LockedUntil):
		return Rule("operator_locked", "Too many failed sign-ins; try again after %s.", o.LockedUntil.UTC().Format("15:04 MST"))
	case o.PasswordHash == "":
		return Rule("password_not_issued", "No password has been issued yet; ask an administrator to reset it.")
	}
	return nil
}

// RecordFailedLogin counts a wrong password and locks the operator after MaxFailedLogins.
func (o *Operator) RecordFailedLogin(now time.Time) {
	o.FailedLogins++
	if o.FailedLogins >= MaxFailedLogins {
		until := now.Add(LockoutDuration)
		o.LockedUntil, o.FailedLogins = &until, 0
	}
}

func (o *Operator) RecordLogin(now time.Time) {
	o.FailedLogins, o.LockedUntil, o.LastLoginAt = 0, nil, &now
}

// SetPassword stores a new password; temporary ones must be changed at the next sign-in.
func (o *Operator) SetPassword(password string, temporary bool, now time.Time) error {
	if err := ValidatePassword(password, o.Username); err != nil {
		return err
	}
	o.PasswordHash, o.MustChangePassword, o.PasswordChangedAt = HashPassword(password), temporary, &now
	o.FailedLogins, o.LockedUntil = 0, nil
	return nil
}

func ValidatePassword(password, username string) error {
	if len(password) < MinPasswordLength || len(password) > 128 {
		return Rule("weak_password", "A password is %d to 128 characters.", MinPasswordLength)
	}
	if username != "" && strings.Contains(strings.ToLower(password), strings.ToLower(username)) {
		return Rule("weak_password", "A password must not contain the username.")
	}
	return nil
}

// Argon2id parameters (OWASP: m=19 MiB, t=2, p=1).
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

// HashPassword hashes a password with Argon2id in PHC string format.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// VerifyPassword checks a password against a PHC Argon2id hash in constant time.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want))) // #nosec G115 -- a 32-byte key
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when the username is unknown, so a sign-in takes as long
// whether or not the operator exists.
var dummyHash = sync.OnceValue(func() string { return HashPassword("not-a-real-password-placeholder") })

func VerifyDummyPassword(password string) { VerifyPassword(password, dummyHash()) }

// TemporaryPassword is a random one-time password an administrator hands over.
func TemporaryPassword() string {
	return RandomString("abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789", 16)
}

// ---------------------------------------------------------------------------------------------
// Sessions

// Session lifetime: signed out after SessionIdle without activity, and after SessionMax whatever.
const (
	SessionIdle = 30 * time.Minute
	SessionMax  = 12 * time.Hour
)

// OperatorSession is a console sign-in. Only the SHA-256 of its token is stored.
type OperatorSession struct {
	ID         uuid.UUID  `db:"id" json:"id"`
	Version    int64      `db:"version" json:"version"`
	OperatorID uuid.UUID  `db:"operator_id" json:"operatorId"`
	TokenHash  string     `db:"token_hash" json:"-"`
	CreatedAt  time.Time  `db:"created_at" json:"createdAt"`
	LastSeenAt time.Time  `db:"last_seen_at" json:"lastSeenAt"`
	ExpiresAt  time.Time  `db:"expires_at" json:"expiresAt"`
	RevokedAt  *time.Time `db:"revoked_at" json:"revokedAt"`
	RemoteAddr string     `db:"remote_addr" json:"remoteAddr"`
	UserAgent  string     `db:"user_agent" json:"userAgent"`
}

// NewSessionToken returns a random session token and the session that stores its hash.
func NewSessionToken(operatorID uuid.UUID, remoteAddr, userAgent string, now time.Time) (string, *OperatorSession) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	if len(userAgent) > 255 {
		userAgent = userAgent[:255]
	}
	return token, &OperatorSession{ID: NewID(), OperatorID: operatorID, TokenHash: TokenHash(token), CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(SessionMax), RemoteAddr: remoteAddr, UserAgent: userAgent}
}

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CSRFToken is derived from the session token: a page that can read it is our own console.
func CSRFToken(token string) string {
	sum := sha256.Sum256([]byte("csrf:" + token))
	return hex.EncodeToString(sum[:16])
}

// Live reports whether the session may still be used.
func (s *OperatorSession) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt) && now.Before(s.LastSeenAt.Add(SessionIdle))
}
