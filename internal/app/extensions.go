package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/extapi"
)

// Extensions (EB.API) is how a bank tailors Zirnis without changing it — T24's local routines.
// An extension is consulted at a hook point (classify or match suspense items, veto a clearing,
// match a nostro statement, extra COB work) and answers with a decision. It is either compiled
// in (Register) or remote (a hook endpoint, maker-checker maintained, called over signed HTTP).
//
// Two rules keep extensions safe:
//   - extensions never post: the core re-validates every decision and applies it itself;
//   - extensions are never called inside a write transaction: with a single serialised writer
//     a slow extension would stall every posting. Callers consult first, then open
//     their unit of work and re-check state.
type Extensions struct {
	a      *App
	client *http.Client

	mu        sync.RWMutex
	local     map[string]extapi.Extension
	localJobs []CobJob
	breakers  map[string]*breaker
}

const extensionsModule = "EB.API"

// breaker is a per-extension circuit: after breakerThreshold consecutive failures calls are
// refused for breakerCoolOff, so a dead endpoint cannot slow every COB job and sweep down.
type breaker struct {
	failures  int
	openUntil time.Time
	lastError string
	lastCall  time.Time
}

const (
	breakerThreshold = 5
	breakerCoolOff   = 30 * time.Second
)

func newExtensions(a *App) *Extensions {
	return &Extensions{a: a, client: &http.Client{}, local: map[string]extapi.Extension{}, breakers: map[string]*breaker{}}
}

// Register compiles an extension into this instance. Its name must be unique across in-process
// extensions and remote endpoints. An extension that runs COB work joins the EB.API module.
func (x *Extensions) Register(ext extapi.Extension) error {
	name := ext.Name()
	if err := domain.ValidateExtensionName(name); err != nil {
		return err
	}
	hooks := extapi.Implements(ext)
	if len(hooks) == 0 {
		return fmt.Errorf("extension %s implements no hook", name)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, dup := x.local[name]; dup {
		return fmt.Errorf("extension %s is already registered", name)
	}
	x.local[name] = ext
	if runner, ok := ext.(extapi.CobRunner); ok {
		x.localJobs = append(x.localJobs, x.cobJob(name, func(ctx context.Context, req extapi.CobJobRequest) (extapi.CobJobResponse, error) {
			return runner.RunCob(ctx, req)
		}, domain.FailClosed))
		x.rebuildModuleJobs(nil)
	}
	return nil
}

// MustRegister is Register for wiring code.
func (x *Extensions) MustRegister(ext extapi.Extension) {
	if err := x.Register(ext); err != nil {
		panic(err)
	}
}

func (x *Extensions) lookupLocal(name string) (extapi.Extension, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	ext, ok := x.local[name]
	return ext, ok
}

// Known reports whether name is an in-process extension or a hook endpoint.
func (x *Extensions) Known(q store.Q, name string) (bool, error) {
	if _, ok := x.lookupLocal(name); ok {
		return true, nil
	}
	return store.Exists(q, "SELECT 1 FROM hook_endpoints WHERE name = ?", name)
}

// ---------------------------------------------------------------------------------------------
// Enquiries

// ExtensionInfo describes one extension and its health.
type ExtensionInfo struct {
	Name          string               `json:"name"`
	Kind          string               `json:"kind"` // InProcess | Remote
	Hooks         []string             `json:"hooks"`
	URL           string               `json:"url,omitempty"`
	SecretEnv     string               `json:"secretEnv,omitempty"`
	TimeoutMs     int                  `json:"timeoutMs,omitempty"`
	FailurePolicy domain.FailurePolicy `json:"failurePolicy"`
	Enabled       bool                 `json:"enabled"`
	Version       int64                `json:"version"`
	Circuit       string               `json:"circuit"`
	Failures      int                  `json:"consecutiveFailures"`
	LastError     string               `json:"lastError,omitempty"`
	LastCallAt    *time.Time           `json:"lastCallAt"`
	SecretPresent *bool                `json:"secretPresent,omitempty"`
}

// List returns every extension, in-process first, then remote, with circuit state.
func (x *Extensions) List(ctx context.Context) ([]ExtensionInfo, error) {
	endpoints, err := x.Endpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := []ExtensionInfo{}
	x.mu.RLock()
	names := make([]string, 0, len(x.local))
	for name := range x.local {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var hooks []string
		for _, h := range extapi.Implements(x.local[name]) {
			hooks = append(hooks, string(h))
		}
		out = append(out, x.withHealth(ExtensionInfo{Name: name, Kind: "InProcess", Hooks: hooks, FailurePolicy: domain.FailClosed, Enabled: true}))
	}
	x.mu.RUnlock()
	for _, e := range endpoints {
		present := os.Getenv(e.SecretEnv) != ""
		x.mu.RLock()
		info := x.withHealth(ExtensionInfo{Name: e.Name, Kind: "Remote", Hooks: e.Hooks, URL: e.URL, SecretEnv: e.SecretEnv, TimeoutMs: e.TimeoutMs, FailurePolicy: e.FailurePolicy,
			Enabled: e.Enabled, Version: e.Version, SecretPresent: &present})
		x.mu.RUnlock()
		out = append(out, info)
	}
	return out, nil
}

// withHealth fills circuit state; the caller holds at least the read lock.
func (x *Extensions) withHealth(info ExtensionInfo) ExtensionInfo {
	info.Circuit = "Closed"
	if b := x.breakers[info.Name]; b != nil {
		info.Failures, info.LastError = b.failures, b.lastError
		if !b.lastCall.IsZero() {
			t := b.lastCall
			info.LastCallAt = &t
		}
		if x.a.now().Before(b.openUntil) {
			info.Circuit = "Open"
		}
	}
	return info
}

func (x *Extensions) Endpoints(ctx context.Context) ([]domain.HookEndpoint, error) {
	return store.Select[domain.HookEndpoint](x.a.read(ctx), "SELECT * FROM hook_endpoints ORDER BY name")
}

// Calls is the extension call log, newest first.
func (x *Extensions) Calls(ctx context.Context, extension string, limit int) ([]domain.HookCall, error) {
	limit = clamp(limit, 1, 500)
	if extension != "" {
		return store.Select[domain.HookCall](x.a.read(ctx), "SELECT * FROM hook_calls WHERE extension = ? ORDER BY called_at DESC LIMIT ?", extension, limit)
	}
	return store.Select[domain.HookCall](x.a.read(ctx), "SELECT * FROM hook_calls ORDER BY called_at DESC LIMIT ?", limit)
}

// ---------------------------------------------------------------------------------------------
// Endpoint maintenance (maker-checker through Parameters)

// KnownHooks lists the hook points as strings.
// "provider" is the provider protocol (pkg/provapi): an endpoint serving it is a provider module.
func KnownHooks() []string {
	out := make([]string, len(extapi.Hooks), len(extapi.Hooks)+1)
	for i, h := range extapi.Hooks {
		out[i] = string(h)
	}
	return append(out, domain.HookProvider)
}

// ProposeEndpoint inputs a new or amended hook endpoint (INAU).
func (p *Parameters) ProposeEndpoint(ctx context.Context, name string, spec domain.HookEndpointSpec) (*domain.ParameterChangeRequest, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := domain.ValidateExtensionName(name); err != nil {
		return nil, err
	}
	if _, ok := p.a.Extensions.lookupLocal(name); ok {
		return nil, domain.Conflict("extension_name_taken", "'%s' is an in-process extension.", name)
	}
	if err := spec.Normalize(KnownHooks()); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamHookEndpoint, name, spec)
}

func (x *Extensions) applyEndpoint(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.HookEndpointSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	if err := spec.Normalize(KnownHooks()); err != nil {
		return err
	}
	endpoint, err := store.Get[domain.HookEndpoint](tx, "SELECT * FROM hook_endpoints WHERE name = ?", request.Key)
	if err != nil {
		return err
	}
	if endpoint == nil {
		if err := tx.Insert("hook_endpoints", domain.NewHookEndpoint(request.Key, spec, tx.Now())); err != nil {
			return err
		}
	} else {
		endpoint.Apply(spec)
		if err := tx.Update("hook_endpoints", endpoint); err != nil {
			return err
		}
	}
	x.mu.Lock()
	delete(x.breakers, request.Key) // a changed endpoint gets a fresh circuit
	x.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------------------------
// Dispatch

type extensionTarget struct {
	name     string
	local    extapi.Extension
	endpoint *domain.HookEndpoint
}

func (t extensionTarget) policy() domain.FailurePolicy {
	if t.endpoint != nil {
		return t.endpoint.FailurePolicy
	}
	return domain.FailClosed // an in-process extension that errors is a bug: stop rather than guess
}

// resolve maps names to the extensions that serve hook. Names that do not serve this hook are
// skipped (a suspense type lists its extensions once for every hook). A name that matches
// nothing at all is an error: a removed endpoint must not silently stop a control.
func (x *Extensions) resolve(q store.Q, names []string, hook extapi.Hook) ([]extensionTarget, error) {
	var targets []extensionTarget
	for _, name := range names {
		if ext, ok := x.lookupLocal(name); ok {
			for _, h := range extapi.Implements(ext) {
				if h == hook {
					targets = append(targets, extensionTarget{name: name, local: ext})
				}
			}
			continue
		}
		endpoint, err := store.Get[domain.HookEndpoint](q, "SELECT * FROM hook_endpoints WHERE name = ?", name)
		if err != nil {
			return nil, err
		}
		if endpoint == nil {
			return nil, domain.Rule("unknown_extension", "Extension '%s' is neither compiled in nor a registered endpoint.", name)
		}
		if endpoint.Enabled && endpoint.Serves(string(hook)) {
			targets = append(targets, extensionTarget{name: name, endpoint: endpoint})
		}
	}
	return targets, nil
}

// Answer is one extension's response.
type Answer[T any] struct {
	Extension string
	Response  T
}

// consult calls every extension in names that serves hook and collects their answers. A failing
// FailOpen extension is logged and skipped; a failing FailClosed extension fails the call.
func consult[Resp any](ctx context.Context, x *Extensions, names []string, hook extapi.Hook, subject string, data any) ([]Answer[Resp], error) {
	if len(names) == 0 {
		return nil, nil
	}
	targets, err := x.resolve(x.a.read(ctx), names, hook)
	if err != nil {
		return nil, err
	}
	var answers []Answer[Resp]
	for _, t := range targets {
		var resp Resp
		if err := x.invoke(ctx, t, hook, subject, data, &resp); err != nil {
			if t.policy() == domain.FailClosed {
				return answers, &ExtensionError{Extension: t.name, Hook: hook, Err: err}
			}
			x.a.Logger.Warn("extension failed; continuing (FailOpen)", "extension", t.name, "hook", hook, "error", err)
			continue
		}
		answers = append(answers, Answer[Resp]{t.name, resp})
	}
	return answers, nil
}

// ExtensionError is a FailClosed extension that could not answer.
type ExtensionError struct {
	Extension string
	Hook      extapi.Hook
	Err       error
}

func (e *ExtensionError) Error() string {
	return fmt.Sprintf("extension %s (%s) failed: %v", e.Extension, e.Hook, e.Err)
}
func (e *ExtensionError) Unwrap() error { return e.Err }

var errCircuitOpen = errors.New("circuit open after repeated failures")

// invoke makes one call — in process or over HTTP — and records it in the call log.
func (x *Extensions) invoke(ctx context.Context, t extensionTarget, hook extapi.Hook, subject string, data any, out any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	businessDate := ""
	if d, err := x.a.Dates.Get(x.a.read(ctx)); err == nil {
		businessDate = d.Today.String()
	}
	req := extapi.Request{APIVersion: extapi.APIVersion, Hook: hook, DeliveryID: domain.NewID().String(), BusinessDate: businessDate, Data: raw}
	started := time.Now()

	if t.local != nil {
		err = x.callLocal(ctx, t.local, req, out)
	} else {
		err = x.callRemote(ctx, t.name, t.endpoint, req, out)
	}
	x.record(ctx, t, hook, subject, req.DeliveryID, time.Since(started), err)
	return err
}

func (x *Extensions) callLocal(ctx context.Context, ext extapi.Extension, req extapi.Request, out any) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("extension panicked: %v", p)
		}
	}()
	resp, err := extapi.Dispatch(ctx, ext, req)
	if err != nil {
		return err
	}
	// Round-trip through JSON so in-process and remote extensions are held to the same contract.
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (x *Extensions) callRemote(ctx context.Context, name string, e *domain.HookEndpoint, req extapi.Request, out any) error {
	x.mu.Lock()
	b := x.breakers[name]
	if b == nil {
		b = &breaker{}
		x.breakers[name] = b
	}
	open := x.a.now().Before(b.openUntil)
	x.mu.Unlock()
	if open {
		return errCircuitOpen
	}
	secret := os.Getenv(e.SecretEnv)
	if secret == "" {
		return fmt.Errorf("secret %s is not set", e.SecretEnv)
	}
	callCtx, cancel := context.WithTimeout(ctx, e.Timeout())
	defer cancel()
	return extapi.Call(callCtx, x.client, e.URL, secret, req, out)
}

func (x *Extensions) record(ctx context.Context, t extensionTarget, hook extapi.Hook, subject, delivery string, latency time.Duration, callErr error) {
	now := x.a.now()
	x.mu.Lock()
	b := x.breakers[t.name]
	if b == nil {
		b = &breaker{}
		x.breakers[t.name] = b
	}
	b.lastCall = now
	switch {
	case callErr == nil:
		b.failures, b.lastError = 0, ""
	case !errors.Is(callErr, errCircuitOpen):
		b.failures++
		b.lastError = callErr.Error()
		if b.failures >= breakerThreshold {
			b.openUntil = now.Add(breakerCoolOff)
		}
	}
	x.mu.Unlock()

	call := domain.HookCall{ID: domain.NewID(), Extension: t.name, Hook: string(hook), Remote: t.endpoint != nil, Subject: subject,
		DeliveryID: delivery, Outcome: "ok", LatencyMs: latency.Milliseconds(), CalledAt: now}
	if callErr != nil {
		call.Outcome = "error"
		msg := callErr.Error()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		call.Error = &msg
	}
	if id := store.Correlation(ctx); id != "" {
		call.CorrelationID = &id
	}
	if err := x.a.db.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error { return tx.Insert("hook_calls", &call) }); err != nil {
		x.a.Logger.Error("could not record extension call", "extension", t.name, "error", err)
	}
}

// ValidateClearing asks every validator for a veto. A veto, or a FailClosed validator that is
// down, rejects the clearing.
func (x *Extensions) ValidateClearing(ctx context.Context, names []string, req extapi.ValidateClearingRequest) error {
	answers, err := consult[extapi.ValidateClearingResponse](ctx, x, names, extapi.SuspenseValidateClearing, req.Item.ID, req)
	var extErr *ExtensionError
	if errors.As(err, &extErr) {
		return domain.Rule("extension_unavailable", "Validation by %s is required but it did not answer.", extErr.Extension)
	}
	if err != nil {
		return err
	}
	for _, a := range answers {
		if !a.Response.Allow {
			code := a.Response.Code
			if code == "" {
				code = "clearing_vetoed"
			}
			msg := a.Response.Message
			if msg == "" {
				msg = "The clearing was rejected."
			}
			return domain.Rule(code, "%s (%s)", msg, a.Extension)
		}
	}
	return nil
}

// ValidateTransaction runs a version's input routines. A veto, or a FailClosed routine that is
// down, refuses the transaction; the overrides they raise join the core's.
func (x *Extensions) ValidateTransaction(ctx context.Context, names []string, subject string, txn extapi.Transaction) ([]domain.Override, error) {
	answers, err := consult[extapi.ValidateTransactionResponse](ctx, x, names, extapi.TransactionValidate, subject, extapi.ValidateTransactionRequest{Transaction: txn})
	var extErr *ExtensionError
	if errors.As(err, &extErr) {
		return nil, domain.Rule("extension_unavailable", "Input routine %s is required but it did not answer.", extErr.Extension)
	}
	if err != nil {
		return nil, err
	}
	var overrides []domain.Override
	for _, a := range answers {
		if !a.Response.Allow {
			return nil, domain.Rule(firstNonEmpty(a.Response.Code, "transaction_vetoed"), "%s (%s)",
				firstNonEmpty(a.Response.Message, "The transaction was refused."), a.Extension)
		}
		for _, o := range a.Response.Overrides {
			code := strings.ToUpper(strings.TrimSpace(o.Code))
			if code == "" {
				continue
			}
			overrides = append(overrides, domain.Override{Code: code, Message: firstNonEmpty(o.Message, code) + " (" + a.Extension + ")"})
		}
	}
	return overrides, nil
}

// AuthoriseTransaction runs a version's authorisation routines: any veto refuses it.
func (x *Extensions) AuthoriseTransaction(ctx context.Context, names []string, subject string, txn extapi.Transaction) error {
	answers, err := consult[extapi.AuthoriseTransactionResponse](ctx, x, names, extapi.TransactionAuthorise, subject, extapi.AuthoriseTransactionRequest{Transaction: txn})
	var extErr *ExtensionError
	if errors.As(err, &extErr) {
		return domain.Rule("extension_unavailable", "Authorisation routine %s is required but it did not answer.", extErr.Extension)
	}
	if err != nil {
		return err
	}
	for _, a := range answers {
		if !a.Response.Allow {
			return domain.Rule(firstNonEmpty(a.Response.Code, "transaction_vetoed"), "%s (%s)",
				firstNonEmpty(a.Response.Message, "The transaction was refused."), a.Extension)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// COB jobs contributed by extensions

func (x *Extensions) cobJob(name string, run func(context.Context, extapi.CobJobRequest) (extapi.CobJobResponse, error), policy domain.FailurePolicy) CobJob {
	return NewCobJob("EXT."+strings.ToUpper(strings.ReplaceAll(name, "-", ".")), extensionsModule, domain.StageApplication,
		func(ctx context.Context, cc CobContext) (CobJobResult, error) {
			resp, err := run(ctx, extapi.CobJobRequest{BusinessDate: cc.BusinessDate.String(), NextBusinessDate: cc.NextBusinessDate.String(), IsMonthEnd: cc.IsMonthEnd})
			if err != nil {
				if policy == domain.FailOpen {
					return CobJobResult{0, "skipped (FailOpen): " + err.Error()}, nil
				}
				return CobJobResult{}, err
			}
			return CobJobResult{resp.Records, resp.Summary}, nil
		})
}

// RefreshCobJobs reloads the remote COB jobs from the endpoint table. It runs at bootstrap,
// whenever an endpoint changes and before every COB starts, so each instance plans COB from
// current parameters.
func (x *Extensions) RefreshCobJobs(ctx context.Context) error {
	endpoints, err := x.Endpoints(ctx)
	if err != nil {
		return err
	}
	var remote []CobJob
	for _, e := range endpoints {
		if !e.Enabled || !e.Serves(string(extapi.CobJob)) {
			continue
		}
		target := extensionTarget{name: e.Name, endpoint: &e}
		remote = append(remote, x.cobJob(e.Name, func(ctx context.Context, req extapi.CobJobRequest) (extapi.CobJobResponse, error) {
			var resp extapi.CobJobResponse
			err := x.invoke(ctx, target, extapi.CobJob, req.BusinessDate, req, &resp)
			return resp, err
		}, e.FailurePolicy))
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.rebuildModuleJobs(remote)
	return nil
}

// rebuildModuleJobs sets the EB.API module's jobs; the caller holds the lock. remote nil keeps
// the remote jobs already planned.
func (x *Extensions) rebuildModuleJobs(remote []CobJob) {
	for _, m := range x.a.modules {
		if m.Code != extensionsModule {
			continue
		}
		if remote == nil {
			for _, j := range m.Jobs {
				if !containsJob(x.localJobs, j.Name()) {
					remote = append(remote, j)
				}
			}
		}
		m.Jobs = append(append([]CobJob{}, x.localJobs...), remote...)
	}
}

func containsJob(jobs []CobJob, name string) bool {
	for _, j := range jobs {
		if j.Name() == name {
			return true
		}
	}
	return false
}
