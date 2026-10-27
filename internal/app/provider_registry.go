package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Providers is the registry of payment providers the core can route to. It holds no provider
// code: every provider is a provider channel (onboarded under maker-checker, with the
// capabilities a checker authorised) served by a module endpoint (also maker-checker). The list
// is rebuilt whenever either is authorised. The sandbox simulator is the one module served in
// process.
type Providers struct {
	a *App

	mu      sync.RWMutex
	local   provapi.Module // in-process module (the sandbox simulator)
	list    []Provider     // routing order: channel priority, then code
	unready map[string]string
}

func newProviders(a *App, local provapi.Module) *Providers {
	return &Providers{a: a, local: local, unready: map[string]string{}}
}

// All lists the providers that can be routed to, in routing order.
func (r *Providers) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.list)
}

// Get returns a provider by name (nil when it is not available).
func (r *Providers) Get(name string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.list {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Unavailable lists provider channels that cannot be routed to right now, with the reason.
func (r *Providers) Unavailable() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.unready))
	for k, v := range r.unready {
		out[k] = v
	}
	return out
}

// localNames lists the providers of the in-process module, in a stable order.
func (r *Providers) localNames() []string { return r.local.Names() }

// Rebuild reloads the providers from their channels and module endpoints.
func (r *Providers) Rebuild(ctx context.Context) error {
	q := r.a.read(ctx)
	channels, err := store.Select[domain.ProviderChannel](q, "SELECT * FROM provider_channels WHERE provider <> '' ORDER BY priority, code")
	if err != nil {
		return err
	}
	endpoints, err := store.Select[domain.HookEndpoint](q, "SELECT * FROM hook_endpoints")
	if err != nil {
		return err
	}
	byName := map[string]*domain.HookEndpoint{}
	for i := range endpoints {
		byName[endpoints[i].Name] = &endpoints[i]
	}
	var list []Provider
	unready := map[string]string{}
	for _, ch := range channels {
		p, reason := r.build(ch, byName)
		if reason != "" {
			unready[ch.Provider] = reason
			continue
		}
		list = append(list, p)
	}
	r.mu.Lock()
	r.list, r.unready = list, unready
	r.mu.Unlock()
	for name, reason := range unready {
		r.a.Logger.Warn("provider not routable", "provider", name, "reason", reason)
	}
	return nil
}

func (r *Providers) build(ch domain.ProviderChannel, endpoints map[string]*domain.HookEndpoint) (Provider, string) {
	switch {
	case !ch.Active:
		return nil, "channel inactive"
	case ch.Module == "":
		if _, ok := r.local[ch.Provider]; !ok || !r.a.Options.Sandbox {
			return nil, "no module named: amend the channel with the module that serves it"
		}
		return &RemoteProvider{name: ch.Provider, caps: ch.Capabilities, transport: provapi.LocalTransport{Module: r.local}}, ""
	}
	t, reason := r.transport(endpoints[ch.Module])
	if reason != "" {
		return nil, reason
	}
	return &RemoteProvider{name: ch.Provider, module: ch.Module, live: true, caps: ch.Capabilities, transport: t}, ""
}

// transport reaches a module endpoint, or says why it cannot.
func (r *Providers) transport(e *domain.HookEndpoint) (provapi.Transport, string) {
	switch {
	case e == nil:
		return nil, "module endpoint not registered"
	case !e.Enabled:
		return nil, fmt.Sprintf("module endpoint %s disabled", e.Name)
	case !e.Serves(domain.HookProvider):
		return nil, fmt.Sprintf("endpoint %s does not serve the provider protocol", e.Name)
	}
	secret := os.Getenv(e.SecretEnv)
	if secret == "" {
		return nil, fmt.Sprintf("secret %s is not set", e.SecretEnv)
	}
	return &provapi.HTTPTransport{URL: e.URL, Secret: secret, Client: &http.Client{Timeout: e.Timeout()}}, ""
}

// Describe asks a module what it serves. It runs outside any transaction.
func (r *Providers) Describe(ctx context.Context, module string) (provapi.Description, error) {
	var t provapi.Transport
	if module == "" {
		if !r.a.Options.Sandbox {
			return provapi.Description{}, domain.Rule("module_required", "Name the module that serves the provider.")
		}
		t = provapi.LocalTransport{Module: r.local}
	} else {
		e, err := store.Get[domain.HookEndpoint](r.a.read(ctx), "SELECT * FROM hook_endpoints WHERE name = ?", module)
		if err != nil {
			return provapi.Description{}, err
		}
		var reason string
		if t, reason = r.transport(e); reason != "" {
			return provapi.Description{}, domain.Rule("module_unavailable", "Module '%s': %s.", module, reason)
		}
	}
	var d provapi.Description
	if _, err := provapi.Call(ctx, t, provapi.OpDescribe, "", domain.NewID().String(), nil, &d); err != nil {
		return d, domain.Rule("module_unavailable", "Module '%s' did not describe itself: %v", module, err)
	}
	return d, nil
}

// capabilitiesFrom converts a module's description of a provider into the core's capabilities.
func capabilitiesFrom(c provapi.Capabilities) domain.ProviderCapabilities {
	out := domain.ProviderCapabilities{Name: c.Name, Deposits: c.Deposits, BankCode: c.BankCode, NotificationValidation: c.NotificationValidation,
		SettlementConfirmed: c.SettlementConfirmed, CheckoutExpirySeconds: c.CheckoutExpirySeconds, IdempotentCharges: c.IdempotentCharges, CheckCharge: c.CheckCharge, CheckPayout: c.CheckPayout,
		CheckTransaction: c.CheckTransaction, RequestStatus: c.RequestStatus, Balance: c.Balance, Webhooks: c.Webhooks}
	for _, col := range c.Collections {
		out.Collections = append(out.Collections, domain.ProviderCollection{Method: domain.PaymentMethod(col.Method), Currencies: col.Currencies, Hosted: col.Hosted})
	}
	for _, p := range c.Payouts {
		out.Payouts = append(out.Payouts, domain.ProviderPayoutRail{Rail: domain.PayoutRail(p.Rail), Currencies: p.Currencies})
	}
	for _, a := range c.Actions {
		out.Actions = append(out.Actions, domain.ProviderAction{Name: a.Name, Label: a.Label, Description: a.Description})
	}
	for _, d := range c.Details {
		out.Details = append(out.Details, domain.ProviderDetail{Name: d.Name, Label: d.Label, Type: domain.LocalFieldType(d.Type), Searchable: d.Searchable})
	}
	return out
}

// describeProvider fills a provider channel spec with what its module says the provider can do.
func (r *Providers) describeProvider(ctx context.Context, spec *domain.ChannelSpec) error {
	d, err := r.Describe(ctx, spec.Module)
	if err != nil {
		return err
	}
	c, ok := d.Providers[spec.Provider]
	if !ok {
		names := make([]string, 0, len(d.Providers))
		for n := range d.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		return domain.Rule("unknown_provider", "Module '%s' does not serve '%s' (it serves %v).", spec.Module, spec.Provider, names)
	}
	caps := capabilitiesFrom(c)
	spec.Capabilities = &caps
	return nil
}

// remote returns a provider as the remote adapter (for webhooks and actions), or why it cannot.
func (r *Providers) remote(name string) (*RemoteProvider, error) {
	p, ok := r.Get(name).(*RemoteProvider)
	if !ok {
		if reason, known := r.Unavailable()[name]; known {
			return nil, domain.Rule("provider_unavailable", "Provider '%s' cannot be reached: %s.", name, reason)
		}
		return nil, domain.NotFound("provider", name)
	}
	return p, nil
}

// sameModule reports whether two providers are served by the same module: a module may speak
// for its own providers, never for another module's.
func (r *Providers) sameModule(a, b string) bool {
	pa, okA := r.Get(a).(*RemoteProvider)
	pb, okB := r.Get(b).(*RemoteProvider)
	return okA && okB && pa.module == pb.module && (pa.module != "" || !pa.live && !pb.live)
}

// RunAction runs an operator action the provider's module offers (e.g. register paybill URLs).
func (r *Providers) RunAction(ctx context.Context, channelCode, action string, params []byte) (any, error) {
	ch, err := r.a.Channels.load(r.a.read(ctx), channelCode)
	if err != nil {
		return nil, err
	}
	if !ch.IsProvider() {
		return nil, domain.Rule("not_a_provider", "Channel %s is not a payment provider's.", ch.Code)
	}
	if !ch.Capabilities.Offers(action) {
		return nil, domain.Rule("unknown_action", "Provider '%s' offers no action '%s'.", ch.Provider, action)
	}
	p, err := r.remote(ch.Provider)
	if err != nil {
		return nil, err
	}
	var out any
	err = r.a.ProviderCalls.Track(ctx, ch.Provider, CallInfo{Operation: "action:" + action, SubjectType: "provider", SubjectID: ch.Provider},
		func(c context.Context) (string, string, error) {
			var err error
			out, err = p.action(c, action, params)
			return "succeeded", "", err
		})
	if err != nil {
		return nil, err
	}
	return out, r.a.db.Atomic(ctx, func(tx *store.Tx) error {
		return tx.Audit("provider.action_run", "provider_channel", ch.Code, map[string]any{"provider": ch.Provider, "action": action})
	})
}
