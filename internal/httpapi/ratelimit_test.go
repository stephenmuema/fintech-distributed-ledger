package httpapi

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestInstancesShareRateLimitsThroughRedis(t *testing.T) {
	url := os.Getenv("ZIRNIS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set ZIRNIS_TEST_REDIS_URL (e.g. redis://localhost:6380/0) to run")
	}
	o, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(o)
	defer client.Close()
	name := "test-" + time.Now().Format("150405.000000000") // a fresh counter for this run
	a := (&Server{redis: client, logger: slog.New(slog.DiscardHandler)}).newLimiter(name, 3)
	b := (&Server{redis: client, logger: slog.New(slog.DiscardHandler)}).newLimiter(name, 3)

	now := time.Now()
	allowed := 0
	for i := range 6 {
		if map[bool]limiter{true: a, false: b}[i%2 == 0].allow("client", now) {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("two instances allowed %d requests against a shared limit of 3", allowed)
	}
}

func TestRateLimitsFallBackToEachInstanceWithoutRedis(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // nothing listens there
	defer client.Close()
	l := (&Server{redis: client, logger: slog.New(slog.DiscardHandler)}).newLimiter("api", 2)

	now := time.Now()
	started := time.Now()
	got := []bool{l.allow("client", now), l.allow("client", now), l.allow("client", now)}
	if !got[0] || !got[1] || got[2] {
		t.Fatalf("the local limit of 2 should apply: %v", got)
	}
	if took := time.Since(started); took > 3*sharedWindowTimeout {
		t.Fatalf("an unreachable Redis slowed every request: %v for three", took)
	}
}
