package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

const testKey = "payments_test_key_with_more_than_32_characters"
const readKey = "payments_read_key_with_more_than_32_characters"

func testRouter(t *testing.T, service *payments.Service, m string, limit int) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	config, _ := json.Marshal([]payments.KeyConfig{{Key: testKey, MerchantID: m, Scopes: []string{"payment_intents:read", "payment_intents:write", "refunds:read", "refunds:write"}}, {Key: readKey, MerchantID: m, Scopes: []string{"payment_intents:read"}}})
	a, e := payments.NewCredentials(string(config), "", "", "")
	if e != nil {
		t.Fatal(e)
	}
	s := NewServer("test", WithPayments(service, a, limit))
	mw, e := s.PaymentMiddleware("/v1")
	if e != nil {
		t.Fatal(e)
	}
	r := gin.New()
	r.ContextWithFallback = true
	r.Use(mw)
	RegisterHandlersWithOptions(r, NewStrictHandler(s, nil), GinServerOptions{BaseURL: "/v1", ErrorHandler: PaymentBindingError})
	return r
}
func call(t *testing.T, r *gin.Engine, method, path, key, idempotency, body string, status int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if idempotency != "" {
		req.Header.Set("Idempotency-Key", idempotency)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("%s %s returned %d want %d: %s", method, path, w.Code, status, w.Body)
	}
	if status == 401 && w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("missing auth challenge")
	}
	if status == 429 && w.Header().Get("Retry-After") == "" {
		t.Fatal("missing retry delay")
	}
	if _, e := uuid.Parse(w.Header().Get("X-Request-Id")); e != nil {
		t.Fatal("missing correlation ID")
	}
	doc, e := GetSwagger()
	if e != nil {
		t.Fatal(e)
	}
	routes, e := legacy.NewRouter(doc)
	if e != nil {
		t.Fatal(e)
	}
	route, params, e := routes.FindRoute(req)
	if e != nil {
		t.Fatal(e)
	}
	input := &openapi3filter.ResponseValidationInput{RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, Route: route, PathParams: params}, Status: w.Code, Header: w.Header(), Options: &openapi3filter.Options{IncludeResponseStatus: true}}
	input.SetBodyBytes(w.Body.Bytes())
	if e = openapi3filter.ValidateResponse(context.Background(), input); e != nil {
		t.Fatalf("response violates contract: %v", e)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	decoder.UseNumber()
	if e = decoder.Decode(&result); e != nil {
		t.Fatal(e)
	}
	return result
}
func TestPaymentsHTTPGuards(t *testing.T) {
	r := testRouter(t, nil, "merchant_test", 100)
	call(t, r, "GET", "/v1/payment-intents", "", "", "", 401)
	call(t, r, "POST", "/v1/payment-intents", readKey, "key", "{}", 403)
	for _, tc := range []struct{ path, body, key string }{
		{"/v1/payment-intents", `{"amount":1,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic"}`, ""},
		{"/v1/payment-intents", `{"amount":0,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic"}`, "key"},
		{"/v1/payment-intents", `{"amount":1,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic","merchant_id":"other"}`, "key"},
		{"/v1/payment-intents", `{"amount":9223372036854775808,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic"}`, "key"},
		{"/v1/payment-intents/pi_demo/capture", `{}`, ""},
	} {
		call(t, r, "POST", tc.path, testKey, tc.key, tc.body, 400)
	}
	for _, path := range []string{"/v1/payment-intents?limit=0", "/v1/payment-intents?limit=101", "/v1/payment-intents?limit=1&limit=2", "/v1/payment-intents?starting_after=pi_a&ending_before=pi_b"} {
		// The last combination is checked by the service; it needs a configured database.
		if strings.Contains(path, "ending_before") {
			continue
		}
		call(t, r, "GET", path, testKey, "", "", 400)
	}
	limited := testRouter(t, nil, "merchant_test", 1)
	call(t, limited, "GET", "/v1/payment-intents", testKey, "", "", 503)
	call(t, limited, "GET", "/v1/payment-intents", testKey, "", "", 429)
}

func TestGatewayNotFound(t *testing.T) {
	router := testRouter(t, nil, "merchant_test", 100)
	router.NoRoute(NotFoundHandler)
	request := httptest.NewRequest("GET", "/v1/not-a-route", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 404 || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("invalid 404 response: %d %v", response.Code, response.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	doc, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Components.Schemas["ProblemDocument"].Value.VisitJSON(body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != float64(404) || body["instance"] != "/v1/not-a-route" || body["type"] != "https://api.stripe-lite.internal/errors/not-found" {
		t.Fatalf("unexpected problem: %v", body)
	}
}
