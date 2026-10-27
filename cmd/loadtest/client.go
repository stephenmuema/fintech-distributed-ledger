package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type client struct {
	base, key string
	http      *http.Client
}

func newClient(base, key string) *client {
	return &client{base: strings.TrimSuffix(base, "/"), key: key, http: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024, IdleConnTimeout: 90 * time.Second},
	}}
}

func (c *client) with(key string) *client { return &client{base: c.base, key: key, http: c.http} }

func (c *client) post(path string, body any) (int, []byte, error) {
	return c.do(http.MethodPost, path, body)
}
func (c *client) get(path string) (int, []byte, error) { return c.do(http.MethodGet, path, nil) }

func (c *client) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", randomHex(16))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	return resp.StatusCode, answer, err
}

// getJSON reads a JSON answer into v, failing on anything but 200.
func (c *client) getJSON(path string, v any) error {
	status, body, err := c.get(path)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, status, body)
	}
	return json.Unmarshal(body, v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
