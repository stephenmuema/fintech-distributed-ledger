package app

import (
	"context"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
	"github.com/stephenmuema/finnie/internal/trace"
)

// CallInfo says what a provider call is for, so its evidence can be traced back to the business
// transaction: the operation, the payment/payout/refund it serves and the idempotency key sent.
type CallInfo struct {
	Operation      string
	SubjectType    string
	SubjectID      string
	IdempotencyKey string
}

type callInfoKey struct{}

// WithCall attaches call information to ctx for the gateway's call log.
func WithCall(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, info)
}

// CallFrom returns the call information attached to ctx.
func CallFrom(ctx context.Context) CallInfo {
	info, _ := ctx.Value(callInfoKey{}).(CallInfo)
	if info.Operation == "" {
		info.Operation = "call"
	}
	return info
}

// ProviderCalls keeps the provider call log — the evidence for "what did we send, and what did
// they answer?" when a transaction fails or is disputed.
type ProviderCalls struct{ a *App }

// Record writes one call in its own transaction, so the evidence survives even when the business
// transaction around it rolls back. Failures to record are logged, never fatal to the payment.
func (p *ProviderCalls) Record(ctx context.Context, call domain.ProviderCall) {
	if call.ID == (domain.ProviderCall{}).ID {
		call.ID = domain.NewID()
	}
	if call.CorrelationID == nil {
		if id := store.Correlation(ctx); id != "" {
			call.CorrelationID = &id
		}
	}
	err := p.a.db.Atomic(context.WithoutCancel(ctx), func(tx *store.Tx) error { return tx.Insert("provider_calls", &call) })
	if err != nil {
		p.a.Logger.Error("could not record provider call", "provider", call.Provider, "operation", call.Operation, "error", err)
	}
}

// NewCall builds a call record from the attempt's context, capture and outcome.
func NewCall(ctx context.Context, provider string, attempt int, started time.Time, capture *trace.Capture, outcome, reference, errorText string) domain.ProviderCall {
	info := CallFrom(ctx)
	call := domain.ProviderCall{
		Provider: provider, Operation: info.Operation, Attempt: attempt, Outcome: outcome,
		Reference: optional(reference), Error: optional(errorText),
		SubjectType: optional(info.SubjectType), SubjectID: optional(info.SubjectID), IdempotencyKey: optional(info.IdempotencyKey),
		DurationMs: time.Since(started).Milliseconds(), StartedAt: started.UTC(),
	}
	if capture != nil {
		if ex, ok := capture.Main(); ok {
			request := ex.Method + " " + ex.URL
			if ex.Request != "" {
				request += "\n" + ex.Request
			}
			call.Request = &request
			if ex.Status != 0 {
				call.ResponseStatus = &ex.Status
			}
			call.Response = optional(ex.Response)
			if ex.Error != "" && call.Error == nil {
				call.Error = &ex.Error
			}
		}
	}
	return call
}

// Track runs a provider call that does not go through the gateway (status queries, balance
// inquiries, URL registration) and records it.
func (p *ProviderCalls) Track(ctx context.Context, provider string, info CallInfo, fn func(context.Context) (outcome, reference string, err error)) error {
	captured, capture := trace.With(WithCall(ctx, info))
	started := time.Now()
	outcome, reference, err := fn(captured)
	errorText := ""
	if err != nil {
		outcome, errorText = "error", err.Error()
	}
	p.Record(captured, NewCall(captured, provider, 1, started, capture, outcome, reference, errorText))
	return err
}

// ForSubject lists the calls made for a business transaction (or provider reference).
func (p *ProviderCalls) ForSubject(ctx context.Context, ids ...string) ([]domain.ProviderCall, error) {
	marks, args := store.In(ids)
	return store.Select[domain.ProviderCall](p.a.read(ctx), "SELECT * FROM provider_calls WHERE subject_id IN ("+marks+") OR reference IN ("+marks+") ORDER BY started_at, attempt",
		append(args, args...)...)
}

// ResultOutcome names a provider result for the call log.
func ResultOutcome(r ProviderResult) string {
	switch r.Outcome {
	case ProviderSucceeded:
		return "succeeded"
	case ProviderPending:
		return "pending"
	}
	return "declined"
}

// checkCharge runs a status query through the call log.
func (a *App) checkCharge(ctx context.Context, provider string, checker StatusChecker, reference, subjectType, subjectID string) (ProviderResult, error) {
	var result ProviderResult
	err := a.ProviderCalls.Track(ctx, provider, CallInfo{Operation: "check_charge", SubjectType: subjectType, SubjectID: subjectID}, func(c context.Context) (string, string, error) {
		var err error
		result, err = checker.CheckCharge(c, reference)
		return ResultOutcome(result), result.Reference, err
	})
	return result, err
}
