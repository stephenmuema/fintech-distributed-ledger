// Package webhook signs and verifies webhook payloads in both directions with HMAC-SHA256.
// The signed payload is "{timestamp}.{body}", so a captured request cannot be replayed outside
// the tolerance window. Header format: t=1700000000,v1=<hex>.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Tolerance is the replay window either side of the signed timestamp.
const Tolerance = 5 * time.Minute

// Compute returns the hex HMAC-SHA256 of "{timestamp}.{body}".
func Compute(secret string, timestamp int64, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// Header builds a signature header value.
func Header(secret string, timestamp int64, body string) string {
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + Compute(secret, timestamp, body)
}

// Verify checks a signature header against the body, in constant time, within the tolerance window.
func Verify(header, secret, body string, now time.Time) bool {
	var timestamp int64
	var signature string
	haveTimestamp := false
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(part, "=")
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
	if !haveTimestamp || signature == "" {
		return false
	}
	if age := now.Sub(time.Unix(timestamp, 0)); age > Tolerance || age < -Tolerance {
		return false
	}
	// Constant-time comparison: don't leak how many leading bytes matched.
	return hmac.Equal([]byte(Compute(secret, timestamp, body)), []byte(signature))
}
