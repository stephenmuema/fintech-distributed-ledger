package domain

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// FailurePolicy says what happens when an extension cannot be reached or errors.
type FailurePolicy string

const (
	// FailClosed blocks the operation: a vetoing hook that is down vetoes, a matching hook that
	// is down fails its COB job (and halts COB).
	FailClosed FailurePolicy = "FailClosed"
	// FailOpen carries on without the extension and records the failure.
	FailOpen FailurePolicy = "FailOpen"
)

var extensionName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// ValidateExtensionName checks an extension or endpoint name (e.g. mpesa-matcher).
func ValidateExtensionName(name string) error {
	if !extensionName.MatchString(name) {
		return Rule("invalid_extension_name", "Extension names are 2–40 characters: lower-case letters, digits and dashes, starting with a letter.")
	}
	return nil
}

var secretEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// HookEndpoint is a remote extension (Python, Go or anything that speaks the extapi protocol).
// It is a parameter record: added and amended under maker-checker, versioned in record history.
// The shared secret is never stored — only the name of the environment variable holding it.
type HookEndpoint struct {
	ID            uuid.UUID     `db:"id" json:"id"`
	Version       int64         `db:"version" json:"version"`
	Name          string        `db:"name" json:"name"`
	URL           string        `db:"url" json:"url"`
	Hooks         StringList    `db:"hooks" json:"hooks"`
	SecretEnv     string        `db:"secret_env" json:"secretEnv"`
	TimeoutMs     int           `db:"timeout_ms" json:"timeoutMs"`
	FailurePolicy FailurePolicy `db:"failure_policy" json:"failurePolicy"`
	Enabled       bool          `db:"enabled" json:"enabled"`
	CreatedAt     time.Time     `db:"created_at" json:"createdAt"`
}

func (e *HookEndpoint) HistoryName() string { return "HookEndpoint" }

func (e *HookEndpoint) Timeout() time.Duration { return time.Duration(e.TimeoutMs) * time.Millisecond }

// Serves reports whether the endpoint is registered for a hook point.
func (e *HookEndpoint) Serves(hook string) bool {
	for _, h := range e.Hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// HookEndpointSpec is what a maker inputs for an endpoint.
type HookEndpointSpec struct {
	URL           string        `json:"url"`
	Hooks         []string      `json:"hooks"`
	SecretEnv     string        `json:"secretEnv"`
	TimeoutMs     int           `json:"timeoutMs"`
	FailurePolicy FailurePolicy `json:"failurePolicy"`
	Enabled       *bool         `json:"enabled,omitempty"`
}

// Normalize validates a spec. known lists the hook points the core offers.
func (s *HookEndpointSpec) Normalize(known []string) error {
	u, err := url.Parse(strings.TrimSpace(s.URL))
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Rule("invalid_hook_endpoint", "URL must be an absolute http or https URL.")
	}
	s.URL = u.String()
	if len(s.Hooks) == 0 {
		return Rule("invalid_hook_endpoint", "An endpoint must serve at least one hook.")
	}
	for _, h := range s.Hooks {
		if !containsString(known, h) {
			return Rule("invalid_hook_endpoint", "Unknown hook '%s' (one of %s).", h, strings.Join(known, ", "))
		}
	}
	s.Hooks = uniqueSorted(s.Hooks)
	if !secretEnvName.MatchString(s.SecretEnv) {
		return Rule("invalid_hook_endpoint", "secretEnv must name an environment variable, e.g. ZIRNIS_HOOK_MPESA_SECRET.")
	}
	if s.TimeoutMs == 0 {
		s.TimeoutMs = 5000
	}
	if s.TimeoutMs < 100 || s.TimeoutMs > 60_000 {
		return Rule("invalid_hook_endpoint", "Timeout must be between 100 ms and 60 s.")
	}
	if s.FailurePolicy == "" {
		s.FailurePolicy = FailOpen
	}
	policy, ok := ParseEnum(string(s.FailurePolicy), FailClosed, FailOpen)
	if !ok {
		return Rule("invalid_hook_endpoint", "Failure policy must be FailClosed or FailOpen.")
	}
	s.FailurePolicy = policy
	return nil
}

func NewHookEndpoint(name string, spec HookEndpointSpec, now time.Time) *HookEndpoint {
	e := &HookEndpoint{ID: NewID(), Name: name, Enabled: true, CreatedAt: now}
	e.Apply(spec)
	return e
}

// Apply amends the endpoint with a normalised spec.
func (e *HookEndpoint) Apply(spec HookEndpointSpec) {
	e.URL, e.Hooks, e.SecretEnv, e.TimeoutMs, e.FailurePolicy = spec.URL, StringList(spec.Hooks), spec.SecretEnv, spec.TimeoutMs, spec.FailurePolicy
	if spec.Enabled != nil {
		e.Enabled = *spec.Enabled
	}
}

// HookCall is one extension call, in-process or remote — the evidence for "what did the
// extension say, and when?" behind every hook-proposed clearing or veto.
type HookCall struct {
	ID            uuid.UUID `db:"id" json:"id"`
	Extension     string    `db:"extension" json:"extension"`
	Hook          string    `db:"hook" json:"hook"`
	Remote        bool      `db:"remote" json:"remote"`
	Subject       string    `db:"subject" json:"subject"`
	DeliveryID    string    `db:"delivery_id" json:"deliveryId"`
	Outcome       string    `db:"outcome" json:"outcome"`
	LatencyMs     int64     `db:"latency_ms" json:"latencyMs"`
	Error         *string   `db:"error" json:"error"`
	CorrelationID *string   `db:"correlation_id" json:"correlationId"`
	CalledAt      time.Time `db:"called_at" json:"calledAt"`
}

func containsString(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
