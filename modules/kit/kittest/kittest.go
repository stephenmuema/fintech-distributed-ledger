// Package kittest helps modules test against fake provider APIs that behave like the real ones
// closely enough to exercise every path: OAuth, request validation, business errors, dropped
// connections and signatures.
package kittest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Recorded is a request the fake received.
type Recorded struct {
	Path string
	Body map[string]any
	Head http.Header
}

// FakeAPI is a provider API served locally.
type FakeAPI struct {
	URL      string
	Mu       sync.Mutex
	requests []Recorded
}

// NewFake serves handle, recording every request.
func NewFake(t testing.TB, handle func(f *FakeAPI, w http.ResponseWriter, r *http.Request, path string, body map[string]any)) *FakeAPI {
	f := &FakeAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		f.Mu.Lock()
		f.requests = append(f.requests, Recorded{r.URL.Path, body, r.Header.Clone()})
		f.Mu.Unlock()
		handle(f, w, r, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// Calls returns the recorded requests to a path (matched by suffix).
func (f *FakeAPI) Calls(path string) []Recorded {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []Recorded
	for _, r := range f.requests {
		if strings.HasSuffix(r.Path, path) {
			out = append(out, r)
		}
	}
	return out
}

// Reply answers with JSON.
func Reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DropConnection simulates a timeout after the request was sent: the outcome is unknown.
func DropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// RSAKeyPair returns a fresh key with its private and public PEM encodings.
func RSAKeyPair(t testing.TB) (*rsa.PrivateKey, string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(private), string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
}
