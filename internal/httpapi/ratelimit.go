package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// limiter decides whether a client may make another request this minute.
type limiter interface {
	allow(key string, now time.Time) bool
}

// newLimiter is the limiter for one set of routes: shared by every instance through Redis when
// ZIRNIS_REDIS_URL is set (ADR 0030), counted by this instance alone otherwise.
func (s *Server) newLimiter(name string, limit int) limiter {
	local := newFixedWindow(limit)
	if s.redis == nil {
		return local
	}
	return &sharedWindow{redis: s.redis, name: name, limit: limit, local: local, logger: s.logger}
}

// sharedWindow is a fixed-window limiter whose counts live in Redis: one counter per client per
// minute, so N instances allow the limit once, not N times. When Redis doesn't answer, the
// instance counts on its own and asks Redis again a few seconds later: Redis is never needed to
// take a request.
type sharedWindow struct {
	redis  *redis.Client
	name   string
	limit  int
	local  *fixedWindow
	logger *slog.Logger
	// retryAt (unix nanoseconds) is when to try Redis again after it failed; 0 while it answers.
	retryAt atomic.Int64
}

const (
	sharedWindowTimeout = 100 * time.Millisecond
	sharedWindowRetry   = 5 * time.Second
)

func (w *sharedWindow) allow(key string, now time.Time) bool {
	if retry := w.retryAt.Load(); retry != 0 && time.Now().UnixNano() < retry {
		return w.local.allow(key, now)
	}
	window := now.Truncate(time.Minute)
	k := fmt.Sprintf("zirnis:rate:%s:%s:%d", w.name, key, window.Unix())
	ctx, cancel := context.WithTimeout(context.Background(), sharedWindowTimeout)
	defer cancel()
	pipe := w.redis.TxPipeline()
	count := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 2*time.Minute)
	if _, err := pipe.Exec(ctx); err != nil {
		if w.retryAt.Swap(time.Now().Add(sharedWindowRetry).UnixNano()) == 0 {
			w.logger.Warn("rate limits counted per instance: Redis did not answer", "error", err)
		}
		return w.local.allow(key, now)
	}
	if w.retryAt.Swap(0) != 0 {
		w.logger.Info("rate limits shared through Redis again")
	}
	return count.Val() <= int64(w.limit)
}
