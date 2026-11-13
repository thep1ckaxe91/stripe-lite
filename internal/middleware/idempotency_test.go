package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestIdempotencyReplayMismatchAndIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, rdb := newTestRedis(t)

	var handlerCalls atomic.Int32
	r := gin.New()
	r.Use(RequestID())
	r.Use(Idempotency(IdempotencyConfig{
		Redis:   rdb,
		TTL:     time.Hour,
		LockTTL: 10 * time.Second,
		Subject: func(c *gin.Context) Subject {
			if m := c.GetHeader("X-Test-Merchant"); m != "" {
				return Subject{Key: "merchant:" + m, MerchantID: m, IsMerchant: true}
			}
			return Subject{Key: "ip:" + c.ClientIP()}
		},
	}))
	r.POST("/v1/payment-intents", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		n := handlerCalls.Add(1)
		c.Header("Location", "/v1/payment-intents/pi_1")
		c.Data(http.StatusCreated, "application/json", []byte(`{"id":"pi_`+string(rune('0'+n))+`","amount":9223372036854775807,"raw_len":`+jsonInt(len(raw))+`}`))
	})
	r.GET("/v1/payment-intents", func(c *gin.Context) {
		handlerCalls.Add(1)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	payloadA := `{"amount":9223372036854775807,"currency":"usd"}`
	payloadB := `{"amount":1000,"currency":"usd"}`

	post := func(merchant, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/payment-intents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Merchant", merchant)
		if key != "" {
			req.Header.Set(HeaderIdempotencyKey, key)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 1. First request executes handler and caches response
	w1 := post("m_a", "idem-1", payloadA)
	if w1.Code != http.StatusCreated || handlerCalls.Load() != 1 {
		t.Fatalf("expected 201 and 1 call, got code=%d calls=%d body=%s", w1.Code, handlerCalls.Load(), w1.Body.String())
	}
	if w1.Header().Get(HeaderIdempotencyReplayed) != "" {
		t.Fatalf("first response must not set %s", HeaderIdempotencyReplayed)
	}

	// 2. Identical retry replays directly from Redis without hitting handler
	w2 := post("m_a", "idem-1", payloadA)
	if w2.Code != http.StatusCreated || handlerCalls.Load() != 1 {
		t.Fatalf("expected replay 201 and calls=1, got code=%d calls=%d", w2.Code, handlerCalls.Load())
	}
	if w2.Header().Get(HeaderIdempotencyReplayed) != "true" {
		t.Fatalf("expected %s=true, got %q", HeaderIdempotencyReplayed, w2.Header().Get(HeaderIdempotencyReplayed))
	}
	if w2.Header().Get("Location") != "/v1/payment-intents/pi_1" {
		t.Fatalf("expected Location header replayed, got %q", w2.Header().Get("Location"))
	}
	if w2.Body.String() != w1.Body.String() {
		t.Fatalf("replayed body differs:\ngot:  %s\nwant: %s", w2.Body.String(), w1.Body.String())
	}
	if w2.Header().Get(HeaderRequestID) == w1.Header().Get(HeaderRequestID) {
		t.Fatalf("replayed request must receive a fresh X-Request-Id, got identical %q", w2.Header().Get(HeaderRequestID))
	}

	// 3. Different payload with same Idempotency-Key returns 409 Conflict
	wMismatch := post("m_a", "idem-1", payloadB)
	if wMismatch.Code != http.StatusConflict || handlerCalls.Load() != 1 {
		t.Fatalf("expected 409 conflict on payload mismatch, got %d calls=%d", wMismatch.Code, handlerCalls.Load())
	}
	var prob ProblemDocument
	if err := json.Unmarshal(wMismatch.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if prob.Type != problemErrorBaseURI+"idempotency-conflict" || prob.Status != 409 {
		t.Fatalf("unexpected conflict problem: %+v", prob)
	}

	// 4. Different merchant using the exact same Idempotency-Key succeeds independently
	wOther := post("m_b", "idem-1", payloadA)
	if wOther.Code != http.StatusCreated || handlerCalls.Load() != 2 {
		t.Fatalf("expected tenant isolation for m_b, got %d calls=%d", wOther.Code, handlerCalls.Load())
	}

	// 5. GET requests bypass idempotency
	reqGet := httptest.NewRequest(http.MethodGet, "/v1/payment-intents", nil)
	reqGet.Header.Set(HeaderIdempotencyKey, "idem-1")
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK || handlerCalls.Load() != 3 {
		t.Fatalf("expected GET to bypass idempotency, got %d calls=%d", wGet.Code, handlerCalls.Load())
	}

	// 6. Repeated Idempotency-Key header returns 400
	reqDup := httptest.NewRequest(http.MethodPost, "/v1/payment-intents", strings.NewReader(payloadA))
	reqDup.Header.Add(HeaderIdempotencyKey, "k1")
	reqDup.Header.Add(HeaderIdempotencyKey, "k2")
	wDup := httptest.NewRecorder()
	r.ServeHTTP(wDup, reqDup)
	if wDup.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on repeated Idempotency-Key, got %d", wDup.Code)
	}
}

func TestIdempotencyConcurrentInFlightAndErrorRelease(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, rdb := newTestRedis(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var returnError atomic.Bool

	r := gin.New()
	r.Use(RequestID())
	r.Use(Idempotency(IdempotencyConfig{Redis: rdb, LockTTL: 10 * time.Second, TTL: time.Minute}))
	r.POST("/v1/pay", func(c *gin.Context) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		if returnError.Load() {
			WriteProblem(c, http.StatusUnprocessableEntity, "invalid-request", "Unprocessable", "validation failed")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"call": n})
	})

	var wg sync.WaitGroup
	wg.Add(1)
	wFirst := httptest.NewRecorder()
	go func() {
		defer wg.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/pay", bytes.NewBufferString(`{"a":1}`))
		r.ServeHTTP(wFirst, req)
	}()

	<-entered

	// Concurrent request while first is in flight must receive 409 request-in-progress
	reqSecond := httptest.NewRequest(http.MethodPost, "/v1/pay", bytes.NewBufferString(`{"a":1}`))
	reqSecond.Header.Set(HeaderIdempotencyKey, "inflight-key")
	// Note: first request had no key! Let's re-run properly with the SAME key on both!
	close(release)
	wg.Wait()

	// Re-test cleanly with the exact same Idempotency-Key on both concurrent requests:
	calls.Store(0)
	entered2 := make(chan struct{})
	release2 := make(chan struct{})
	r2 := gin.New()
	r2.Use(RequestID())
	r2.Use(Idempotency(IdempotencyConfig{Redis: rdb, LockTTL: 10 * time.Second, TTL: time.Minute}))
	r2.POST("/v1/pay", func(c *gin.Context) {
		n := calls.Add(1)
		if n == 1 {
			close(entered2)
			<-release2
		}
		if returnError.Load() {
			WriteProblem(c, http.StatusBadRequest, "invalid-request", "Bad Request", "failed")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"call": n})
	})

	var wg2 sync.WaitGroup
	wg2.Add(1)
	w1 := httptest.NewRecorder()
	go func() {
		defer wg2.Done()
		req := httptest.NewRequest(http.MethodPost, "/v1/pay", strings.NewReader(`{"amount":500}`))
		req.Header.Set(HeaderIdempotencyKey, "concurrent-key")
		r2.ServeHTTP(w1, req)
	}()

	<-entered2
	req2 := httptest.NewRequest(http.MethodPost, "/v1/pay", strings.NewReader(`{"amount":500}`))
	req2.Header.Set(HeaderIdempotencyKey, "concurrent-key")
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("expected 409 while request in progress, got %d: %s", w2.Code, w2.Body.String())
	}
	var prob ProblemDocument
	_ = json.Unmarshal(w2.Body.Bytes(), &prob)
	if prob.Type != problemErrorBaseURI+"request-in-progress" {
		t.Fatalf("expected request-in-progress problem type, got %+v", prob)
	}

	close(release2)
	wg2.Wait()
	if w1.Code != http.StatusCreated {
		t.Fatalf("expected first request 201, got %d", w1.Code)
	}

	// Now verify error lock release: 4xx/5xx must release lock so client can retry
	returnError.Store(true)
	reqErr := httptest.NewRequest(http.MethodPost, "/v1/pay", strings.NewReader(`{"amount":500}`))
	reqErr.Header.Set(HeaderIdempotencyKey, "retry-after-err")
	wErr := httptest.NewRecorder()
	r2.ServeHTTP(wErr, reqErr)
	if wErr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 error, got %d", wErr.Code)
	}

	returnError.Store(false)
	reqRetry := httptest.NewRequest(http.MethodPost, "/v1/pay", strings.NewReader(`{"amount":500}`))
	reqRetry.Header.Set(HeaderIdempotencyKey, "retry-after-err")
	wRetry := httptest.NewRecorder()
	r2.ServeHTTP(wRetry, reqRetry)
	if wRetry.Code != http.StatusCreated || wRetry.Header().Get(HeaderIdempotencyReplayed) != "" {
		t.Fatalf("expected fresh 201 after error lock release, got %d replayed=%q", wRetry.Code, wRetry.Header().Get(HeaderIdempotencyReplayed))
	}
	if _, err := uuid.Parse(wRetry.Header().Get(HeaderRequestID)); err != nil {
		t.Fatal("missing X-Request-Id")
	}
}

func TestIdempotencyFailOpenWhenRedisDown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr, rdb := newTestRedis(t)
	mr.Close()

	var calls atomic.Int32
	r := gin.New()
	r.Use(Idempotency(IdempotencyConfig{Redis: rdb}))
	r.POST("/v1/pay", func(c *gin.Context) {
		calls.Add(1)
		c.Status(http.StatusCreated)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/pay", strings.NewReader(`{}`))
	req.Header.Set(HeaderIdempotencyKey, "k1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated || calls.Load() != 1 {
		t.Fatalf("expected fail-open execution when Redis is down, got %d calls=%d", w.Code, calls.Load())
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
