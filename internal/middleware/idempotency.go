package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// HeaderIdempotencyKey is the client-supplied deduplication key header.
	HeaderIdempotencyKey = "Idempotency-Key"
	// HeaderIdempotencyReplayed is attached when a response is served from Redis cache.
	HeaderIdempotencyReplayed = "X-Idempotency-Replayed"

	inProgressPrefix          = "IN_PROGRESS:"
	defaultMaxRequestBody     = 1 << 20 // 1 MiB
	defaultMaxResponseBody    = 2 << 20 // 2 MiB
	maxIdempotencyKeyLength   = 255
	redisCleanupTimeout       = 2 * time.Second
)

// releaseIdempotencyLockScript deletes the key only if the caller still owns the lock token.
var releaseIdempotencyLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

// commitIdempotencyScript replaces the in-progress marker with the cached response snapshot
// only if the caller still holds the exact lock token.
var commitIdempotencyScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	redis.call('SET', KEYS[1], ARGV[2], 'PX', tonumber(ARGV[3]))
	return 1
end
return 0
`)

type idempotencyRecord struct {
	State       string            `json:"state"`
	RequestHash string            `json:"request_hash"`
	StatusCode  int               `json:"status_code"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        []byte            `json:"body"`
}

// IdempotencyConfig configures the gateway Redis idempotency middleware.
type IdempotencyConfig struct {
	Redis        redis.Cmdable
	KeyPrefix    string
	TTL          time.Duration
	LockTTL      time.Duration
	MaxBodyBytes int64
	Subject      SubjectResolver
}

// Idempotency returns a Gin middleware enforcing Two-Tier Redis idempotency on POST requests.
// On cache hit with matching SHA-256 request payload hash, it replays the cached response directly
// from Redis with X-Idempotency-Replayed: true. On payload mismatch or concurrent execution,
// it aborts with RFC 7807 409 Conflict.
func Idempotency(cfg IdempotencyConfig) gin.HandlerFunc {
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "idempotency:"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = 30 * time.Second
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxRequestBody
	}
	if cfg.Subject == nil {
		cfg.Subject = NewSubjectResolver(nil)
	}

	return func(c *gin.Context) {
		EnsureRequestID(c)
		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		keys := c.Request.Header.Values(HeaderIdempotencyKey)
		if len(keys) == 0 {
			c.Next()
			return
		}
		if len(keys) > 1 {
			WriteProblem(c, http.StatusBadRequest, "invalid-request", "Bad Request", "The idempotency key must not be repeated.")
			return
		}

		idempotencyKey := strings.TrimSpace(keys[0])
		if idempotencyKey == "" || len(idempotencyKey) > maxIdempotencyKeyLength {
			WriteProblem(c, http.StatusBadRequest, "invalid-request", "Bad Request", "The Idempotency-Key header must be between 1 and 255 characters.")
			return
		}

		// When Redis is unconfigured, allow traffic through to durable DB-level fallback.
		if cfg.Redis == nil {
			c.Next()
			return
		}

		rawBody, err := readAndRestoreBody(c.Request, cfg.MaxBodyBytes)
		if err != nil {
			WriteProblem(c, http.StatusBadRequest, "invalid-request", "Bad Request", "The request body is invalid or too large.")
			return
		}
		requestHash := sha256Hex(rawBody)

		subject := cfg.Subject(c)
		redisKey := fmt.Sprintf("%s%s:%s:%s:%s", cfg.KeyPrefix, subject.Key, c.Request.Method, c.Request.URL.Path, idempotencyKey)
		lockValue := inProgressPrefix + uuid.NewString()

		acquired, err := cfg.Redis.SetNX(c.Request.Context(), redisKey, lockValue, cfg.LockTTL).Result()
		if err != nil {
			log.Printf("[Middleware] idempotency SetNX Redis error (fail-open): %v", err)
			c.Next()
			return
		}

		if !acquired {
			existing, getErr := cfg.Redis.Get(c.Request.Context(), redisKey).Result()
			if getErr == redis.Nil {
				// Lock expired or was released between SetNX and Get; attempt re-acquisition once.
				acquired, err = cfg.Redis.SetNX(c.Request.Context(), redisKey, lockValue, cfg.LockTTL).Result()
				if err != nil {
					log.Printf("[Middleware] idempotency retry SetNX error (fail-open): %v", err)
					c.Next()
					return
				}
				if !acquired {
					existing, getErr = cfg.Redis.Get(c.Request.Context(), redisKey).Result()
				}
			}

			if !acquired {
				if getErr != nil {
					log.Printf("[Middleware] idempotency Get Redis error (fail-open): %v", getErr)
					c.Next()
					return
				}
				handleExistingRecord(c, existing, requestHash)
				return
			}
		}

		committed := false
		defer func() {
			if committed {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), redisCleanupTimeout)
			defer cancel()
			if err := releaseIdempotencyLockScript.Run(cleanupCtx, cfg.Redis, []string{redisKey}, lockValue).Err(); err != nil {
				log.Printf("[Middleware] idempotency lock release error: %v", err)
			}
		}()

		recorder := &idempotencyResponseRecorder{
			ResponseWriter: c.Writer,
			maxBytes:       defaultMaxResponseBody,
		}
		c.Writer = recorder

		c.Next()

		status := recorder.Status()
		if status >= http.StatusOK && status < http.StatusMultipleChoices && !recorder.overflowed {
			snapshot := idempotencyRecord{
				State:       "completed",
				RequestHash: requestHash,
				StatusCode:  status,
				ContentType: recorder.Header().Get("Content-Type"),
				Headers:     extractCacheableHeaders(recorder.Header()),
				Body:        recorder.body.Bytes(),
			}
			encoded, marshalErr := json.Marshal(snapshot)
			if marshalErr == nil {
				saveCtx, cancel := context.WithTimeout(context.Background(), redisCleanupTimeout)
				defer cancel()
				if err := commitIdempotencyScript.Run(saveCtx, cfg.Redis, []string{redisKey}, lockValue, encoded, cfg.TTL.Milliseconds()).Err(); err != nil {
					log.Printf("[Middleware] idempotency commit error: %v", err)
				} else {
					committed = true
				}
			}
		}
	}
}

func handleExistingRecord(c *gin.Context, rawValue, incomingHash string) {
	if strings.HasPrefix(rawValue, inProgressPrefix) {
		WriteProblem(
			c,
			http.StatusConflict,
			"request-in-progress",
			"Conflict",
			"A request with this idempotency key is still being processed. Retry with the same key and payload.",
		)
		return
	}

	var record idempotencyRecord
	if err := json.Unmarshal([]byte(rawValue), &record); err != nil {
		log.Printf("[Middleware] idempotency unmarshal corrupt record (fail-open): %v", err)
		c.Next()
		return
	}

	if record.RequestHash != incomingHash {
		WriteProblem(
			c,
			http.StatusConflict,
			"idempotency-conflict",
			"Conflict",
			"This idempotency key is already bound to a different request payload.",
		)
		return
	}

	for k, v := range record.Headers {
		c.Header(k, v)
	}
	c.Header(HeaderIdempotencyReplayed, "true")

	contentType := record.ContentType
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	c.Data(record.StatusCode, contentType, record.Body)
	c.Abort()
}

func readAndRestoreBody(r *http.Request, maxBytes int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxBytes)
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func extractCacheableHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	saved := make(map[string]string)
	for _, key := range []string{"Location", "Cache-Control"} {
		if v := h.Get(key); v != "" {
			saved[key] = v
		}
	}
	if len(saved) == 0 {
		return nil
	}
	return saved
}

type idempotencyResponseRecorder struct {
	gin.ResponseWriter
	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
	maxBytes    int
	overflowed  bool
}

func (r *idempotencyResponseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.statusCode = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *idempotencyResponseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if !r.overflowed {
		if r.body.Len()+len(b) <= r.maxBytes {
			r.body.Write(b)
		} else {
			r.overflowed = true
			r.body.Reset()
		}
	}
	return r.ResponseWriter.Write(b)
}

func (r *idempotencyResponseRecorder) WriteString(s string) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if !r.overflowed {
		if r.body.Len()+len(s) <= r.maxBytes {
			r.body.WriteString(s)
		} else {
			r.overflowed = true
			r.body.Reset()
		}
	}
	return r.ResponseWriter.WriteString(s)
}

func (r *idempotencyResponseRecorder) Status() int {
	if r.wroteHeader {
		return r.statusCode
	}
	return r.ResponseWriter.Status()
}
