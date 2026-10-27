// Package trace captures the HTTP exchanges made while calling a provider, so they can be kept as
// evidence in the provider call log — with secrets redacted and phone numbers masked first.
package trace

import (
	"context"
	"strings"
	"sync"

	"github.com/stephenmuema/finnie/pkg/provapi"
)

// Exchange is one HTTP request/response with a provider.
type Exchange struct {
	Method   string `json:"method"`
	URL      string `json:"url"`
	Status   int    `json:"status,omitempty"`
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Capture collects the exchanges made under a context.
type Capture struct {
	mu        sync.Mutex
	exchanges []Exchange
}

type captureKey struct{}

// With returns a context whose provider HTTP exchanges are captured.
func With(ctx context.Context) (context.Context, *Capture) {
	c := &Capture{}
	return context.WithValue(ctx, captureKey{}, c), c
}

// Record adds an exchange to the context's capture, if any. Bodies are redacted here, at the
// source, so secrets never reach storage.
func Record(ctx context.Context, e Exchange) {
	c, ok := ctx.Value(captureKey{}).(*Capture)
	if !ok {
		return
	}
	if i := strings.IndexByte(e.URL, '?'); i >= 0 {
		e.URL = e.URL[:i] // query strings can carry credentials
	}
	e.Request, e.Response = Redact(e.Request), Redact(e.Response)
	c.mu.Lock()
	c.exchanges = append(c.exchanges, e)
	c.mu.Unlock()
}

// Main returns the business exchange — the last one that is not an OAuth token request.
func (c *Capture) Main() (Exchange, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.exchanges) - 1; i >= 0; i-- {
		u := strings.ToLower(c.exchanges[i].URL)
		if !strings.Contains(u, "oauth") && !strings.Contains(u, "/token") && !strings.Contains(u, "authenticate") {
			return c.exchanges[i], true
		}
	}
	return Exchange{}, false
}

// Redact replaces secret-bearing JSON fields and masks phone numbers; non-JSON bodies are kept
// (truncated). The result is safe to store and to show to operators.
func Redact(body string) string { return provapi.Redact(body) }

// MaskPhone keeps only the country/trunk prefix and the last three digits of a Kenyan mobile
// number, e.g. 254712345678 → 254******678.
func MaskPhone(s string) string { return provapi.MaskPhone(s) }
