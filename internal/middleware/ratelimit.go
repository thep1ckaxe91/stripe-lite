package middleware

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// HeaderRateLimitLimit exposes the maximum allowed requests in the active sliding window.
	HeaderRateLimitLimit = "X-RateLimit-Limit"
	// HeaderRateLimitRemaining exposes the remaining requests allowed in the active sliding window.
	HeaderRateLimitRemaining = "X-RateLimit-Remaining"
	// HeaderRateLimitReset exposes the Unix epoch seconds timestamp when the oldest entry expires.
	HeaderRateLimitReset = "X-RateLimit-Reset"
	// HeaderRetryAfter specifies the whole seconds delay before retrying after a 429 rejection.
	HeaderRetryAfter = "Retry-After"
)

// slidingWindowScript enforces an atomic sliding window log in Redis using a sorted set.
// KEYS[1] = ratelimit:{subject}
// ARGV[1] = now_ms, ARGV[2] = window_ms, ARGV[3] = limit, ARGV[4] = unique_member
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
local member = ARGV[4]
local cutoff = now - window

redis.call('ZREMRANGEBYSCORE', key, '-inf', cutoff)
local count = redis.call('ZCARD', key)
local allowed = 0

if count < limit then
	redis.call('ZADD', key, now, member)
	count = count + 1
	allowed = 1
end

redis.call('PEXPIRE', key, window)

local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local reset_ms = now + window
if #oldest >= 2 then
	reset_ms = tonumber(oldest[2]) + window
end

local remaining = limit - count
if remaining < 0 then
	remaining = 0
end

return {allowed, remaining, reset_ms}
`)

// QuotaResolver returns the (limit, window) quota applicable to a caller subject.
// Sprint 3 business tiering plugs into this hook without altering limiter internals.
type QuotaResolver func(ctx context.Context, subject Subject) (limit int, window time.Duration)

// SkipFunc determines whether a request should bypass a gateway middleware.
type SkipFunc func(c *gin.Context) bool

// RateLimitConfig configures the sliding window rate limiter middleware.
type RateLimitConfig struct {
	Redis        redis.Cmdable
	KeyPrefix    string
	DefaultLimit int
	Window       time.Duration
	Quota        QuotaResolver
	Subject      SubjectResolver
	Skip         SkipFunc
	Now          func() time.Time
	BasePath     string
}

type rateLimitDecision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	ResetAt    time.Time
	RetryAfter time.Duration
}

// RateLimit returns a Gin middleware enforcing a sliding window rate limit per subject.
// When Redis is configured, limits are evaluated atomically via Lua scripts.
// When Redis is nil, an in-memory sliding window log is used as a local fallback.
func RateLimit(cfg RateLimitConfig) gin.HandlerFunc {
	if cfg.DefaultLimit <= 0 {
		cfg.DefaultLimit = 120
	}
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "ratelimit:"
	}
	if cfg.Subject == nil {
		cfg.Subject = NewSubjectResolver(nil)
	}
	if cfg.Quota == nil {
		defaultLimit := cfg.DefaultLimit
		defaultWindow := cfg.Window
		cfg.Quota = func(context.Context, Subject) (int, time.Duration) {
			return defaultLimit, defaultWindow
		}
	}
	if cfg.Skip == nil {
		cfg.Skip = defaultRateLimitSkip(cfg.BasePath)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	var mem *memorySlidingWindow
	if cfg.Redis == nil {
		mem = newMemorySlidingWindow()
	}

	return func(c *gin.Context) {
		EnsureRequestID(c)
		if cfg.Skip != nil && cfg.Skip(c) {
			c.Next()
			return
		}

		subject := cfg.Subject(c)
		limit, window := cfg.Quota(c.Request.Context(), subject)
		if limit <= 0 || window <= 0 {
			c.Next()
			return
		}

		now := cfg.Now()
		var decision rateLimitDecision
		var err error

		if cfg.Redis != nil {
			decision, err = evalRedisSlidingWindow(c.Request.Context(), cfg.Redis, cfg.KeyPrefix+subject.Key, now, window, limit)
			if err != nil {
				// Fail-open on transient Redis connectivity loss so payment traffic is not halted.
				log.Printf("[Middleware] rate limit Redis error (fail-open): %v", err)
				c.Next()
				return
			}
		} else {
			decision = mem.Allow(subject.Key, now, window, limit)
		}

		c.Header(HeaderRateLimitLimit, strconv.Itoa(decision.Limit))
		c.Header(HeaderRateLimitRemaining, strconv.Itoa(decision.Remaining))
		c.Header(HeaderRateLimitReset, strconv.FormatInt(ceilUnixSeconds(decision.ResetAt), 10))

		if !decision.Allowed {
			retryAfterSec := int64(math.Ceil(decision.RetryAfter.Seconds()))
			if retryAfterSec < 1 {
				retryAfterSec = 1
			}
			c.Header(HeaderRetryAfter, strconv.FormatInt(retryAfterSec, 10))
			WriteProblem(
				c,
				http.StatusTooManyRequests,
				"rate-limited",
				"Too Many Requests",
				"The request limit was reached. Retry later.",
			)
			return
		}

		c.Next()
	}
}

func evalRedisSlidingWindow(
	ctx context.Context,
	client redis.Cmdable,
	redisKey string,
	now time.Time,
	window time.Duration,
	limit int,
) (rateLimitDecision, error) {
	nowMs := now.UnixMilli()
	windowMs := window.Milliseconds()
	if windowMs < 1 {
		windowMs = 1
	}
	member := fmt.Sprintf("%d:%s", nowMs, uuid.NewString())

	raw, err := slidingWindowScript.Run(ctx, client, []string{redisKey}, nowMs, windowMs, limit, member).Slice()
	if err != nil {
		return rateLimitDecision{}, err
	}
	if len(raw) != 3 {
		return rateLimitDecision{}, fmt.Errorf("unexpected sliding window lua result length: %d", len(raw))
	}

	allowedVal, err := toInt64(raw[0])
	if err != nil {
		return rateLimitDecision{}, err
	}
	remainingVal, err := toInt64(raw[1])
	if err != nil {
		return rateLimitDecision{}, err
	}
	resetMs, err := toInt64(raw[2])
	if err != nil {
		return rateLimitDecision{}, err
	}

	resetAt := time.UnixMilli(resetMs)
	retryAfter := resetAt.Sub(now)
	if retryAfter < time.Second {
		retryAfter = time.Second
	}

	return rateLimitDecision{
		Allowed:    allowedVal == 1,
		Limit:      limit,
		Remaining:  int(remainingVal),
		ResetAt:    resetAt,
		RetryAfter: retryAfter,
	}, nil
}

func toInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case string:
		return strconv.ParseInt(n, 10, 64)
	default:
		return 0, fmt.Errorf("unexpected numeric type %T in lua response", v)
	}
}

func ceilUnixSeconds(t time.Time) int64 {
	sec := t.Unix()
	if t.Nanosecond() > 0 {
		sec++
	}
	return sec
}

func defaultRateLimitSkip(basePath string) SkipFunc {
	base := strings.TrimSuffix(basePath, "/")
	return func(c *gin.Context) bool {
		p := c.Request.URL.Path
		switch p {
		case "/healthz", "/docs", "/swagger", "/openapi.json", "/openapi.yaml":
			return true
		}
		if base != "" && p == base+"/healthz" {
			return true
		}
		return false
	}
}

type memorySlidingWindow struct {
	mu      sync.Mutex
	entries map[string][]int64
}

func newMemorySlidingWindow() *memorySlidingWindow {
	return &memorySlidingWindow{
		entries: make(map[string][]int64),
	}
}

func (m *memorySlidingWindow) Allow(key string, now time.Time, window time.Duration, limit int) rateLimitDecision {
	nowMs := now.UnixMilli()
	windowMs := window.Milliseconds()
	if windowMs < 1 {
		windowMs = 1
	}
	cutoff := nowMs - windowMs

	m.mu.Lock()
	defer m.mu.Unlock()

	timestamps := m.entries[key]
	pruned := timestamps[:0]
	for _, ts := range timestamps {
		if ts > cutoff {
			pruned = append(pruned, ts)
		}
	}

	allowed := false
	if len(pruned) < limit {
		pruned = append(pruned, nowMs)
		allowed = true
	}
	m.entries[key] = pruned

	resetMs := nowMs + windowMs
	if len(pruned) > 0 {
		resetMs = pruned[0] + windowMs
	}
	remaining := limit - len(pruned)
	if remaining < 0 {
		remaining = 0
	}

	resetAt := time.UnixMilli(resetMs)
	retryAfter := resetAt.Sub(now)
	if retryAfter < time.Second {
		retryAfter = time.Second
	}

	return rateLimitDecision{
		Allowed:    allowed,
		Limit:      limit,
		Remaining:  remaining,
		ResetAt:    resetAt,
		RetryAfter: retryAfter,
	}
}
