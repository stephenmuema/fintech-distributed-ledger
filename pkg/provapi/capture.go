package provapi

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// Exchange is one HTTP request/response between a module and its provider.
type Exchange struct {
	Method   string `json:"method"`
	URL      string `json:"url"`
	Status   int    `json:"status,omitempty"`
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

type capture struct {
	mu        sync.Mutex
	exchanges []Exchange
}

type captureKey struct{}

func withCapture(ctx context.Context) (context.Context, *capture) {
	c := &capture{}
	return context.WithValue(ctx, captureKey{}, c), c
}

func (c *capture) list() []Exchange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Exchange(nil), c.exchanges...)
}

// Record keeps an HTTP exchange with the provider as evidence for the request being served.
// Query strings are dropped and bodies redacted here, so secrets never leave the module.
func Record(ctx context.Context, e Exchange) {
	c, ok := ctx.Value(captureKey{}).(*capture)
	if !ok {
		return
	}
	if i := strings.IndexByte(e.URL, '?'); i >= 0 {
		e.URL = e.URL[:i]
	}
	e.Request, e.Response = Redact(e.Request), Redact(e.Response)
	c.mu.Lock()
	c.exchanges = append(c.exchanges, e)
	c.mu.Unlock()
}

const maxBody = 8 << 10

var (
	sensitiveKey = regexp.MustCompile(`(?i)pass|secret|credential|token|authorization|signature|pin`)
	phoneNumber  = regexp.MustCompile(`^\+?(254|0)(7|1)\d{8}$`)
)

// Redact replaces secret-bearing JSON fields and masks phone numbers; non-JSON bodies are kept
// (truncated). The result is safe to store and to show to operators.
func Redact(body string) string {
	if body == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return truncateBody(body)
	}
	b, err := json.Marshal(redactValue("", v))
	if err != nil {
		return truncateBody(body)
	}
	return truncateBody(string(b))
}

func redactValue(key string, v any) any {
	if key != "" && sensitiveKey.MatchString(key) {
		return "[REDACTED]"
	}
	switch t := v.(type) {
	case map[string]any:
		for k, inner := range t {
			t[k] = redactValue(k, inner)
		}
		return t
	case []any:
		for i, inner := range t {
			t[i] = redactValue("", inner)
		}
		return t
	case string:
		return MaskPhone(t)
	case float64:
		if s := strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), ".0"), ".00"); phoneNumber.MatchString(s) {
			return MaskPhone(s)
		}
	}
	return v
}

// MaskPhone keeps only the country/trunk prefix and the last three digits of a Kenyan mobile
// number, e.g. 254712345678 → 254******678.
func MaskPhone(s string) string {
	if !phoneNumber.MatchString(s) {
		return s
	}
	keep := 3
	switch {
	case strings.HasPrefix(s, "+"):
		keep = 4
	case strings.HasPrefix(s, "0"):
		keep = 1
	}
	return s[:keep] + strings.Repeat("*", len(s)-keep-3) + s[len(s)-3:]
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func truncateBody(s string) string {
	if len(s) <= maxBody {
		return s
	}
	return s[:maxBody] + "…[truncated]"
}
