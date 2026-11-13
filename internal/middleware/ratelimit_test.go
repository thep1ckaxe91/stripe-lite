package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func TestRateLimitRedisSlidingWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, rdb := newTestRedis(t)

	currentTime := time.Unix(1700000000, 0)
	var mu sync.Mutex
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return currentTime
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		currentTime = currentTime.Add(d)
	}

	r := gin.New()
	r.Use(RequestID())
	r.Use(RateLimit(RateLimitConfig{
		Redis:        rdb,
		DefaultLimit: 2,
		Window:       60 * time.Second,
		BasePath:     "/v1",
		Now:          nowFn,
		Subject: func(c *gin.Context) Subject {
			if m := c.GetHeader("X-Test-Merchant"); m != "" {
				return Subject{Key: "merchant:" + m, MerchantID: m, IsMerchant: true}
			}
			return Subject{Key: "ip:" + c.ClientIP()}
		},
	}))
	r.GET("/v1/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/v1/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	doReq := func(merchant, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if merchant != "" {
			req.Header.Set("X-Test-Merchant", merchant)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Request 1 at t=0s: allowed, remaining=1
	w1 := doReq("m_alpha", "/v1/ping")
	if w1.Code != http.StatusOK || w1.Header().Get(HeaderRateLimitLimit) != "2" || w1.Header().Get(HeaderRateLimitRemaining) != "1" {
		t.Fatalf("unexpected req 1 response: code=%d headers=%v", w1.Code, w1.Header())
	}

	// Advance to t=40s: Request 2 allowed, remaining=0
	advance(40 * time.Second)
	w2 := doReq("m_alpha", "/v1/ping")
	if w2.Code != http.StatusOK || w2.Header().Get(HeaderRateLimitRemaining) != "0" {
		t.Fatalf("unexpected req 2 response: code=%d headers=%v", w2.Code, w2.Header())
	}

	// Advance to t=45s: Request 3 rejected with 429 RFC 7807 + Retry-After
	advance(5 * time.Second)
	w3 := doReq("m_alpha", "/v1/ping")
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w3.Code, w3.Body.String())
	}
	if w3.Header().Get("Content-Type") != problemContentType {
		t.Fatalf("expected problem+json content type, got %q", w3.Header().Get("Content-Type"))
	}
	retryAfter, err := strconv.Atoi(w3.Header().Get(HeaderRetryAfter))
	if err != nil || retryAfter < 1 {
		t.Fatalf("expected integer Retry-After >= 1, got %q", w3.Header().Get(HeaderRetryAfter))
	}
	if _, err := uuid.Parse(w3.Header().Get(HeaderRequestID)); err != nil {
		t.Fatalf("missing valid X-Request-Id on 429: %v", err)
	}
	var prob ProblemDocument
	if err := json.Unmarshal(w3.Body.Bytes(), &prob); err != nil {
		t.Fatalf("invalid problem JSON: %v", err)
	}
	if prob.Status != 429 || prob.Type != problemErrorBaseURI+"rate-limited" {
		t.Fatalf("unexpected problem payload: %+v", prob)
	}

	// Different merchant is isolated and unaffected
	wOther := doReq("m_beta", "/v1/ping")
	if wOther.Code != http.StatusOK || wOther.Header().Get(HeaderRateLimitRemaining) != "1" {
		t.Fatalf("expected isolated quota for m_beta, got %d remaining=%q", wOther.Code, wOther.Header().Get(HeaderRateLimitRemaining))
	}

	// Advance to t=61s: t=0s slot expired, t=40s slot remains -> 1 more request allowed, next blocked!
	advance(16 * time.Second)
	w4 := doReq("m_alpha", "/v1/ping")
	if w4.Code != http.StatusOK || w4.Header().Get(HeaderRateLimitRemaining) != "0" {
		t.Fatalf("expected sliding window to free t=0 slot at t=61s, got %d remaining=%q", w4.Code, w4.Header().Get(HeaderRateLimitRemaining))
	}
	w5 := doReq("m_alpha", "/v1/ping")
	if w5.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 because t=40s and t=61s slots fill limit=2, got %d", w5.Code)
	}

	// Health endpoint bypasses rate limiting
	for i := 0; i < 5; i++ {
		wh := doReq("m_alpha", "/v1/healthz")
		if wh.Code != http.StatusOK {
			t.Fatalf("healthz should bypass rate limiter, got %d", wh.Code)
		}
	}
}

func TestRateLimitRedisConcurrencyAndTiering(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, rdb := newTestRedis(t)

	r := gin.New()
	r.Use(RateLimit(RateLimitConfig{
		Redis:  rdb,
		Window: time.Minute,
		Quota: func(_ context.Context, s Subject) (int, time.Duration) {
			if s.MerchantID == "m_pro" {
				return 15, time.Minute
			}
			return 5, time.Minute
		},
		Subject: func(c *gin.Context) Subject {
			m := c.GetHeader("X-Test-Merchant")
			return Subject{Key: "merchant:" + m, MerchantID: m, IsMerchant: true}
		},
	}))
	r.GET("/v1/items", func(c *gin.Context) { c.Status(http.StatusOK) })

	var okCount, blockedCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/v1/items", nil)
			req.Header.Set("X-Test-Merchant", "m_free")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			switch w.Code {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusTooManyRequests:
				blockedCount.Add(1)
			default:
				t.Errorf("unexpected status %d", w.Code)
			}
		}()
	}
	wg.Wait()

	if okCount.Load() != 5 || blockedCount.Load() != 7 {
		t.Fatalf("expected atomic limit=5 allowed and 7 blocked, got allowed=%d blocked=%d", okCount.Load(), blockedCount.Load())
	}

	// Pro merchant gets 15 quota
	req := httptest.NewRequest(http.MethodGet, "/v1/items", nil)
	req.Header.Set("X-Test-Merchant", "m_pro")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get(HeaderRateLimitLimit) != "15" || w.Header().Get(HeaderRateLimitRemaining) != "14" {
		t.Fatalf("unexpected pro tier response: code=%d headers=%v", w.Code, w.Header())
	}
}

func TestRateLimitFailOpenAndMemoryFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Case 1: Redis fails -> fail-open
	mr, rdb := newTestRedis(t)
	rFailOpen := gin.New()
	rFailOpen.Use(RateLimit(RateLimitConfig{Redis: rdb, DefaultLimit: 1, Window: time.Minute}))
	rFailOpen.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })
	mr.Close()

	w := httptest.NewRecorder()
	rFailOpen.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected fail-open 200 when Redis is down, got %d", w.Code)
	}

	// Case 2: Memory fallback when Redis == nil
	now := time.Unix(1700000000, 0)
	rMem := gin.New()
	rMem.Use(RateLimit(RateLimitConfig{
		DefaultLimit: 1,
		Window:       10 * time.Second,
		Now:          func() time.Time { return now },
		Subject:      NewSubjectResolver(nil),
	}))
	rMem.GET("/v1/ping", func(c *gin.Context) { c.Status(http.StatusOK) })

	w1 := httptest.NewRecorder()
	rMem.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w1.Code != http.StatusOK {
		t.Fatalf("expected 200 on first memory-limited call, got %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	rMem.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/v1/ping", nil))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on second memory-limited call, got %d", w2.Code)
	}
}

func TestSubjectResolverPopulatesPrincipalOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "test_key_with_sufficient_length_32"
	cfg, _ := json.Marshal([]payments.KeyConfig{{Key: key, MerchantID: "m_auth", Scopes: []string{"payment_intents:read"}}})
	creds, err := payments.NewCredentials(string(cfg), "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	resolver := NewSubjectResolver(creds)
	var sub Subject
	var ctxPrincipal payments.Principal
	var hasCtxPrincipal bool

	r := gin.New()
	r.GET("/test", func(c *gin.Context) {
		sub = resolver(c)
		ctxPrincipal, hasCtxPrincipal = payments.PrincipalFrom(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-API-Key", key)
	r.ServeHTTP(httptest.NewRecorder(), req)

	if !sub.IsMerchant || sub.MerchantID != "m_auth" || sub.Key != "merchant:m_auth" {
		t.Fatalf("unexpected merchant subject: %+v", sub)
	}
	if !hasCtxPrincipal || ctxPrincipal.MerchantID != "m_auth" {
		t.Fatalf("expected principal stored on request context")
	}
}
