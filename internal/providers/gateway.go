// Package providers holds what the core needs around provider modules: the resilience gateway
// every provider call runs through, and the sandbox simulator (cards, mobile money, a bank rail,
// FX rates and a KYC vendor). No real provider's code lives here: providers are modules.
package providers

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/clock"
	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/metrics"
	"github.com/stephenmuema/finnie/internal/trace"
)

// ResilienceOptions tune the gateway.
type ResilienceOptions struct {
	Timeout           time.Duration
	RetryCount        int
	RetryBaseDelay    time.Duration
	FailureRatio      float64
	MinimumThroughput int
	SamplingWindow    time.Duration
	BreakDuration     time.Duration
}

func DefaultResilience() ResilienceOptions {
	return ResilienceOptions{
		Timeout: 5 * time.Second, RetryCount: 2, RetryBaseDelay: 200 * time.Millisecond,
		FailureRatio: 0.5, MinimumThroughput: 4, SamplingWindow: 30 * time.Second, BreakDuration: 30 * time.Second,
	}
}

// Gateway runs every provider call through: retry (exponential backoff + jitter) → circuit
// breaker → per-attempt timeout. Retries are only safe because every request carries a stable
// provider idempotency key, and only for failures where the request was not processed; a request
// whose outcome is unknown (app.Ambiguous) is never retried. When a provider's circuit is open,
// calls fail fast and the router fails over to the next provider.
//
// A timeout is different: the request was sent and may have been acted on. Retrying it at the
// same provider is safe (the idempotency key makes the provider answer the original request), so
// the gateway does. Failing over is not, so once an attempt has timed out the call can only end
// with an answer or as app.Ambiguous: another provider would collect or pay the money twice.
type Gateway struct {
	opts     ResilienceOptions
	clock    clock.Clock
	metrics  *metrics.Registry
	logger   *slog.Logger
	mu       sync.Mutex
	breakers map[string]*breaker
	// Outages, when set, lets operators drill a provider outage (sandbox) for any connector.
	Outages interface{ IsDown(string) bool }
	// Recorder, when set, keeps every attempt in the provider call log.
	Recorder interface {
		Record(ctx context.Context, call domain.ProviderCall)
	}
}

func NewGateway(opts ResilienceOptions, clk clock.Clock, m *metrics.Registry, logger *slog.Logger) *Gateway {
	return &Gateway{opts: opts, clock: clk, metrics: m, logger: logger, breakers: map[string]*breaker{}}
}

func (g *Gateway) breaker(provider string) *breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.breakers[provider]
	if !ok {
		b = &breaker{}
		g.breakers[provider] = b
	}
	return b
}

func (g *Gateway) Execute(ctx context.Context, provider string, call func(context.Context) (app.ProviderResult, error)) (app.ProviderResult, error) {
	started := time.Now()
	outcome := "ok"
	defer func() {
		g.metrics.ProviderCalls.Observe(float64(time.Since(started).Milliseconds()), "provider", provider, "outcome", outcome)
	}()

	if g.Outages != nil && g.Outages.IsDown(provider) {
		outcome = "provider_unavailable"
		err := app.Unavailable(provider, "provider_unavailable")
		g.log(ctx, provider, 1, time.Now(), nil, app.ProviderResult{}, err)
		return app.ProviderResult{}, err
	}
	b := g.breaker(provider)
	sent := false // an attempt timed out: the provider may have acted on the request
	unknown := func(err error) error {
		if sent {
			outcome = "ambiguous"
			return app.Ambiguous(provider, "timeout")
		}
		return err
	}
	for attempt := 0; ; attempt++ {
		if !b.allow(g.clock.Now(), g.opts) {
			outcome = "circuit_open"
			err := app.Unavailable(provider, "circuit_open")
			g.log(ctx, provider, attempt+1, time.Now(), nil, app.ProviderResult{}, err)
			return app.ProviderResult{}, unknown(err)
		}

		captured, capture := trace.With(ctx)
		attemptCtx, cancel := context.WithTimeout(captured, g.opts.Timeout)
		attemptStarted := time.Now()
		result, err := call(attemptCtx)
		timedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if timedOut && err != nil {
			if u, ok := app.AsUnavailable(err); !ok || !u.Ambiguous {
				err, sent = app.Unavailable(provider, "timeout"), true
			}
		}
		// A result the connector did return is kept even when the deadline passed: a transfer it
		// reports as Pending ("sent, outcome unknown") must never be turned into a retryable timeout,
		// or the retry — or the next rail — would pay it twice.
		g.log(captured, provider, attempt+1, attemptStarted, capture, result, err)

		unavailable, transient := app.AsUnavailable(err)
		if transient && unavailable.Reroute {
			b.record(g.clock.Now(), false, g.opts) // the provider answered; it is healthy
			outcome = unavailable.Code
			return result, unknown(err)
		}
		if transient && unavailable.Ambiguous {
			b.record(g.clock.Now(), true, g.opts)
			outcome = "ambiguous"
			return result, err // may have been processed: never retried
		}
		if err != nil && !transient {
			outcome = "error"
			return result, err // a programming or context error: not the provider's fault
		}
		if opened := b.record(g.clock.Now(), transient, g.opts); opened {
			g.logger.Error("circuit OPEN — traffic will fail over", "provider", provider)
		}
		if !transient {
			return result, nil
		}

		outcome = unavailable.Code
		if attempt >= g.opts.RetryCount || ctx.Err() != nil {
			return app.ProviderResult{}, unknown(err)
		}
		delay := g.opts.RetryBaseDelay << attempt
		delay += time.Duration(domain.RandomInt(int64(delay)/2 + 1)) // jitter
		g.logger.Warn("retrying provider", "provider", provider, "attempt", attempt+2, "error", err)
		select {
		case <-ctx.Done():
			return app.ProviderResult{}, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (g *Gateway) State(provider string) app.CircuitState {
	g.mu.Lock()
	b, ok := g.breakers[provider]
	g.mu.Unlock()
	if !ok {
		return app.CircuitClosed
	}
	return b.state(g.clock.Now(), g.opts)
}

// breaker is a failure-ratio circuit breaker over a sampling window.
type breaker struct {
	mu          sync.Mutex
	windowStart time.Time
	calls       int
	failures    int
	openedAt    time.Time
	open        bool
	trial       bool // half-open probe in flight
}

func (b *breaker) state(now time.Time, o ResilienceOptions) app.CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case !b.open:
		return app.CircuitClosed
	case now.Sub(b.openedAt) >= o.BreakDuration:
		return app.CircuitHalfOpen
	}
	return app.CircuitOpen
}

func (b *breaker) allow(now time.Time, o ResilienceOptions) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	if now.Sub(b.openedAt) < o.BreakDuration || b.trial {
		return false
	}
	b.trial = true // half-open: let one probe through
	return true
}

// record notes an attempt's outcome and reports whether the circuit just opened.
func (b *breaker) record(now time.Time, failed bool, o ResilienceOptions) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.open {
		b.trial = false
		if failed {
			b.openedAt = now // probe failed: stay open for another break
			return false
		}
		b.open, b.calls, b.failures, b.windowStart = false, 0, 0, now
		return false
	}
	if now.Sub(b.windowStart) > o.SamplingWindow {
		b.windowStart, b.calls, b.failures = now, 0, 0
	}
	b.calls++
	if failed {
		b.failures++
	}
	if b.calls >= o.MinimumThroughput && float64(b.failures)/float64(b.calls) >= o.FailureRatio {
		b.open, b.openedAt = true, now
		return true
	}
	return false
}

// log records one attempt in the provider call log (when a recorder is configured).
func (g *Gateway) log(ctx context.Context, provider string, attempt int, started time.Time, capture *trace.Capture, result app.ProviderResult, err error) {
	if g.Recorder == nil {
		return
	}
	outcome, reference, errorText := app.ResultOutcome(result), result.Reference, result.DeclineCode
	if err != nil {
		reference, errorText = "", err.Error()
		outcome = "error"
		if u, ok := app.AsUnavailable(err); ok {
			outcome = "unavailable"
			switch {
			case u.Reroute:
				outcome = "reroute"
			case u.Ambiguous:
				outcome = "ambiguous"
			}
		}
	}
	g.Recorder.Record(ctx, app.NewCall(ctx, provider, attempt, started, capture, outcome, reference, errorText))
}
