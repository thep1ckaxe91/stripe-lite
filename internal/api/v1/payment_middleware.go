package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

type paymentProblem struct {
	body   ProblemDocument
	status int
}

func problem(ctx context.Context, err error) paymentProblem {
	e := payments.NormalizeError(err)
	instance := "/requests/" + uuid.NewString()
	if c, ok := ctx.(*gin.Context); ok {
		instance = "/requests/" + c.Writer.Header().Get("X-Request-Id")
	}
	return paymentProblem{ProblemDocument{Type: "https://api.stripe-lite.internal/errors/" + e.Code, Title: http.StatusText(e.Status), Status: int32(e.Status), Detail: &e.Detail, Instance: &instance}, e.Status}
}
func (p paymentProblem) write(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/problem+json")
	if p.status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="payments"`)
	}
	if p.status == 429 {
		w.Header().Set("Retry-After", "60")
	}
	w.WriteHeader(p.status)
	return json.NewEncoder(w).Encode(p.body)
}
func abortPayment(c *gin.Context, err error) { c.Abort(); _ = problem(c, err).write(c.Writer) }

// PaymentMiddleware validates credentials and requests before the generated handlers run.
func (s *Server) PaymentMiddleware(basePath string) (gin.HandlerFunc, error) {
	doc, err := GetSwagger()
	if err != nil {
		return nil, err
	}
	doc.Servers = openapi3.Servers{&openapi3.Server{URL: basePath}}
	routes, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, err
	}
	type bucket struct {
		count int
		until time.Time
	}
	var mu sync.Mutex
	buckets := map[string]bucket{}
	nextSweep := time.Now().Add(time.Minute)
	return func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, basePath)
		if path != "/payment-intents" && !strings.HasPrefix(path, "/payment-intents/") && path != "/refunds" && !strings.HasPrefix(path, "/refunds/") {
			c.Next()
			return
		}
		requestID := uuid.NewString()
		if id, e := uuid.Parse(c.GetHeader("X-Request-Id")); e == nil {
			requestID = id.String()
		}
		c.Header("X-Request-Id", requestID)
		var p payments.Principal
		var authErr error
		if s.auth == nil {
			authErr = payments.ErrUnauthorized
		} else {
			p, authErr = s.auth.Authenticate(c.Request)
		}
		if authErr != nil {
			abortPayment(c, payments.ErrUnauthorized)
			return
		}
		scope := "payment_intents:"
		if strings.HasPrefix(path, "/refunds") {
			scope = "refunds:"
		}
		if c.Request.Method == http.MethodGet {
			scope += "read"
		} else {
			scope += "write"
		}
		if !p.Scopes[scope] {
			abortPayment(c, payments.Fail(403, "forbidden", "The credential does not have the required scope."))
			return
		}
		now := time.Now()
		mu.Lock()
		if now.After(nextSweep) {
			for key, b := range buckets {
				if !now.Before(b.until) {
					delete(buckets, key)
				}
			}
			nextSweep = now.Add(time.Minute)
		}
		b := buckets[p.MerchantID]
		if !now.Before(b.until) {
			b = bucket{until: now.Add(time.Minute)}
		}
		b.count++
		buckets[p.MerchantID] = b
		mu.Unlock()
		if b.count > s.rateLimit {
			abortPayment(c, payments.Fail(429, "rate-limited", "The request limit was reached. Retry later."))
			return
		}
		c.Request = c.Request.WithContext(payments.WithPrincipal(c.Request.Context(), p))
		route, params, e := routes.FindRoute(c.Request)
		if e != nil {
			abortPayment(c, payments.ErrNotFound)
			return
		}
		// Read a bounded copy. Both validation and generated binding consume the body.
		if c.Request.Body != nil {
			body, e := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20+1))
			_ = c.Request.Body.Close()
			if e != nil || len(body) > 1<<20 {
				abortPayment(c, payments.Fail(400, "invalid-request", "The request body is invalid or too large."))
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			if route.Operation.RequestBody == nil && len(body) > 0 {
				abortPayment(c, payments.Fail(400, "invalid-request", "This operation does not accept a request body."))
				return
			}
		}
		// Reject repeated scalar parameters instead of silently selecting one value.
		for _, values := range c.Request.URL.Query() {
			if len(values) > 1 {
				abortPayment(c, payments.Fail(400, "invalid-request", "Query parameters must not be repeated."))
				return
			}
		}
		if len(c.Request.Header.Values("Idempotency-Key")) > 1 {
			abortPayment(c, payments.Fail(400, "invalid-request", "The idempotency key must not be repeated."))
			return
		}
		input := &openapi3filter.RequestValidationInput{Request: c.Request, PathParams: params, Route: route, Options: &openapi3filter.Options{SkipSettingDefaults: true, AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
		if e = openapi3filter.ValidateRequest(c.Request.Context(), input); e != nil {
			abortPayment(c, payments.Fail(400, "invalid-request", "The request does not match the API contract."))
			return
		}
		c.Next()
		if len(c.Errors) > 0 && !c.Writer.Written() {
			abortPayment(c, payments.Fail(400, "invalid-request", "The request could not be read."))
		}
	}, nil
}

// PaymentBindingError keeps generated parameter errors in the common error format.
func PaymentBindingError(c *gin.Context, err error, status int) {
	if status < 400 || status > 599 {
		status = 400
	}
	abortPayment(c, payments.Fail(status, "invalid-request", fmt.Sprintf("The request is invalid (HTTP %s).", strconv.Itoa(status))))
}

func (p paymentProblem) VisitCreatePaymentIntentResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p paymentProblem) VisitGetPaymentIntentResponse(w http.ResponseWriter) error { return p.write(w) }

func (p paymentProblem) VisitConfirmPaymentIntentResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p paymentProblem) VisitCapturePaymentIntentResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p paymentProblem) VisitCancelPaymentIntentResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p paymentProblem) VisitListPaymentIntentsResponse(w http.ResponseWriter) error {
	return p.write(w)
}

func (p paymentProblem) VisitCreateRefundResponse(w http.ResponseWriter) error { return p.write(w) }

func (p paymentProblem) VisitGetRefundResponse(w http.ResponseWriter) error { return p.write(w) }

func (p paymentProblem) VisitListRefundsResponse(w http.ResponseWriter) error { return p.write(w) }
