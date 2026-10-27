package extapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Wire protocol. The core POSTs a Request to the endpoint URL with:
//
//	X-Zirnis-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">
//	X-Zirnis-Hook:      the hook point
//	X-Zirnis-Delivery:  the delivery id
//
// and expects 200 with the hook's response JSON. Anything else is a failed call.
const (
	SignatureHeader = "X-Zirnis-Signature"
	HookHeader      = "X-Zirnis-Hook"
	DeliveryHeader  = "X-Zirnis-Delivery"
)

// Tolerance is the replay window either side of the signed timestamp.
const Tolerance = 5 * time.Minute

// MaxBody caps request and response bodies.
const MaxBody = 4 << 20

// Sign returns the signature header value for body at timestamp.
func Sign(secret string, timestamp int64, body []byte) string {
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + mac(secret, timestamp, body)
}

func mac(secret string, timestamp int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(timestamp, 10) + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a signature header in constant time, within the tolerance window.
func Verify(header, secret string, body []byte, now time.Time) bool {
	var timestamp int64
	var signature string
	haveTimestamp := false
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch key {
		case "t":
			if t, err := strconv.ParseInt(value, 10, 64); err == nil {
				timestamp, haveTimestamp = t, true
			}
		case "v1":
			signature = value
		}
	}
	if secret == "" || !haveTimestamp || signature == "" {
		return false
	}
	if age := now.Sub(time.Unix(timestamp, 0)); age > Tolerance || age < -Tolerance {
		return false
	}
	return hmac.Equal([]byte(mac(secret, timestamp, body)), []byte(signature))
}

// Call sends one hook request and decodes the response into out.
func Call(ctx context.Context, client *http.Client, url, secret string, req Request, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SignatureHeader, Sign(secret, time.Now().Unix(), body))
	httpReq.Header.Set(HookHeader, string(req.Hook))
	httpReq.Header.Set(DeliveryHeader, req.DeliveryID)
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("invalid response: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Handler serves an extension over HTTP so the core can call it as a remote hook. Requests
// must carry a valid signature made with secret.
func Handler(ext Extension, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
		if err != nil {
			http.Error(w, "could not read body", http.StatusBadRequest)
			return
		}
		if !Verify(r.Header.Get(SignatureHeader), secret, body, time.Now()) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		var req Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		out, err := Dispatch(r.Context(), ext, req)
		if errors.Is(err, ErrNotImplemented) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// ErrNotImplemented means the extension does not serve the requested hook.
var ErrNotImplemented = errors.New("hook not implemented by this extension")

// Dispatch routes a request to the extension's implementation of its hook.
func Dispatch(ctx context.Context, ext any, req Request) (any, error) {
	switch req.Hook {
	case SuspenseOpened:
		if x, ok := ext.(FundsHandler); ok {
			return call(ctx, req.Data, x.OnFunds)
		}
	case SuspenseClassify:
		if x, ok := ext.(Classifier); ok {
			return call(ctx, req.Data, x.Classify)
		}
	case SuspenseMatch:
		if x, ok := ext.(Matcher); ok {
			return call(ctx, req.Data, x.Match)
		}
	case SuspenseValidateClearing:
		if x, ok := ext.(ClearingValidator); ok {
			return call(ctx, req.Data, x.ValidateClearing)
		}
	case NostroMatch:
		if x, ok := ext.(StatementMatcher); ok {
			return call(ctx, req.Data, x.MatchStatement)
		}
	case CobJob:
		if x, ok := ext.(CobRunner); ok {
			return call(ctx, req.Data, x.RunCob)
		}
	case TransactionValidate:
		if x, ok := ext.(TransactionValidator); ok {
			return call(ctx, req.Data, x.ValidateTransaction)
		}
	case TransactionAuthorise:
		if x, ok := ext.(TransactionAuthoriser); ok {
			return call(ctx, req.Data, x.AuthoriseTransaction)
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotImplemented, req.Hook)
}

func call[Req, Resp any](ctx context.Context, data json.RawMessage, fn func(context.Context, Req) (Resp, error)) (any, error) {
	var in Req
	if len(data) > 0 {
		if err := json.Unmarshal(data, &in); err != nil {
			return nil, fmt.Errorf("malformed %T: %w", in, err)
		}
	}
	return fn(ctx, in)
}

// Client is how a remote extension calls back into the core: to book money a provider sent
// (Inward) and to report the outcome of an item it held (ReportOutcome). Requests are signed with
// the extension's shared secret; the core acts as "hook:<name>".
type Client struct {
	BaseURL string
	Name    string
	Secret  string
	HTTP    *http.Client
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/webhooks/extensions/"+c.Name+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, Sign(c.Secret, time.Now().Unix(), raw))
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Inward books funds the provider sent into the channel's inward suspense account.
func (c *Client) Inward(ctx context.Context, channel string, funds InwardFunds) (map[string]any, error) {
	var out map[string]any
	return out, c.post(ctx, "/channels/"+channel+"/inward", funds, &out)
}

// ReportOutcome settles an item the extension held (or never answered for).
func (c *Client) ReportOutcome(ctx context.Context, itemID string, outcome FundsOutcome) (map[string]any, error) {
	var out map[string]any
	return out, c.post(ctx, "/items/"+itemID+"/outcome", outcome, &out)
}
