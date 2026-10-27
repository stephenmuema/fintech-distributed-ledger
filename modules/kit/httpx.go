// Package kit is what provider modules share: JSON calls with failure classification that is safe
// for money movement, cached OAuth tokens, RSA signing, currency amounts, and the few lines that
// serve a module (Serve). It holds no provider's code.
package kit

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/stephenmuema/finnie/pkg/provapi"
)

// ErrAmbiguous means the request may or may not have been processed (timeout after sending, a
// dropped connection, a gateway 5xx). Disbursements must never be re-sent on this error — the
// caller parks them as pending and lets the provider's callback or an operator decide.
var ErrAmbiguous = errors.New("provider outcome unknown")

// NotDeliveredError means the request never reached the provider (DNS, refused connection, TLS),
// so retrying or failing over is safe.
type NotDeliveredError struct{ Err error }

func (e *NotDeliveredError) Error() string { return "request not delivered: " + e.Err.Error() }
func (e *NotDeliveredError) Unwrap() error { return e.Err }

// StatusError is a definite HTTP answer from the provider (4xx, or a 5xx carrying a business error).
type StatusError struct {
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(string(e.Body), 300))
}

// Client performs JSON calls against one provider.
type Client struct {
	HTTP *http.Client
}

func New(timeout time.Duration) *Client {
	return &Client{HTTP: &http.Client{Timeout: timeout}}
}

// Do sends a request and decodes a JSON response into out (when non-nil). Non-2xx responses
// return *StatusError with the body, so callers can map provider error codes.
func (c *Client) Do(ctx context.Context, method, url string, headers map[string]string, body, out any) error {
	var reader io.Reader
	var sent string
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case string: // pre-encoded form body
		reader, contentType = strings.NewReader(b), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		reader, sent = bytes.NewReader(raw), string(raw)
	}
	exchange := provapi.Exchange{Method: method, URL: url, Request: sent}
	defer func() { provapi.Record(ctx, exchange) }()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if reader != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		exchange.Error = err.Error()
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return &NotDeliveredError{err}
		}
		var dns *net.DNSError
		if errors.As(err, &dns) {
			return &NotDeliveredError{err}
		}
		return fmt.Errorf("%w: %v", ErrAmbiguous, err)
	}
	defer resp.Body.Close()
	exchange.Status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: reading response: %v", ErrAmbiguous, err)
	}
	exchange.Response = string(raw)
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests:
		return &NotDeliveredError{fmt.Errorf("HTTP %d", resp.StatusCode)} // refused before processing
	case resp.StatusCode >= 500 && !looksLikeJSON(raw):
		return fmt.Errorf("%w: HTTP %d", ErrAmbiguous, resp.StatusCode) // gateway error, outcome unknown
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &StatusError{resp.StatusCode, raw}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: undecodable response: %v", ErrAmbiguous, err)
		}
	}
	return nil
}

func looksLikeJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Tokens caches an OAuth access token and refreshes it a minute before it expires.
type Tokens struct {
	Fetch   func(ctx context.Context) (token string, ttl time.Duration, err error)
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (t *Tokens) Get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.expires) {
		return t.token, nil
	}
	token, ttl, err := t.Fetch(ctx)
	if err != nil {
		return "", err
	}
	if ttl <= 2*time.Minute {
		ttl = 2 * time.Minute
	}
	t.token, t.expires = token, time.Now().Add(ttl-time.Minute)
	return token, nil
}

// Invalidate forces a refresh on the next call (after a 401).
func (t *Tokens) Invalidate() {
	t.mu.Lock()
	t.token = ""
	t.mu.Unlock()
}

// BasicAuth builds an HTTP Basic credential.
func BasicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// SignSHA256 signs data with RSA PKCS#1 v1.5 over SHA-256 and returns base64.
func SignSHA256(key *rsa.PrivateKey, data string) (string, error) {
	digest := sha256.Sum256([]byte(data))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifySHA256 checks a base64 RSA PKCS#1 v1.5 / SHA-256 signature over data.
func VerifySHA256(key *rsa.PublicKey, data []byte, signatureB64 string) bool {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureB64))
	if err != nil || key == nil {
		return false
	}
	digest := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) == nil
}

// LoadPrivateKey reads a PEM RSA private key (PKCS#1 or PKCS#8) from a file path or inline PEM.
func LoadPrivateKey(pathOrPEM string) (*rsa.PrivateKey, error) {
	block, err := pemBlock(pathOrPEM)
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}

// LoadPublicKey reads a PEM RSA public key or certificate from a file path or inline PEM.
func LoadPublicKey(pathOrPEM string) (*rsa.PublicKey, error) {
	block, err := pemBlock(pathOrPEM)
	if err != nil {
		return nil, err
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if key, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not RSA")
	}
	return key, nil
}

func pemBlock(pathOrPEM string) (*pem.Block, error) {
	raw := []byte(pathOrPEM)
	if !strings.Contains(pathOrPEM, "-----BEGIN") {
		b, err := os.ReadFile(pathOrPEM) // #nosec G304 -- operator-supplied key path
		if err != nil {
			return nil, err
		}
		raw = b
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	return block, nil
}

// MustJSON is json.Marshal for values known to encode.
func MustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
