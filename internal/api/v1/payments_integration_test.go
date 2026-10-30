//go:build integration

package v1

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

func TestPaymentsHTTPLifecycle(t *testing.T) {
	uri := os.Getenv("PAYMENTS_TEST_DATABASE_URL")
	if uri == "" {
		t.Fatal("PAYMENTS_TEST_DATABASE_URL is required; use make test-integration")
	}
	db, e := pgxpool.New(context.Background(), uri)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	m := "merchant_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	service := payments.New(db, payments.DemoDirectory{Merchant: m, CustomerID: "cus_demo"}, payments.SimulatedProcessor{})
	r := testRouter(t, service, m, 1000)
	payload := `{"amount":2000,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic","payment_method_id":"pm_demo"}`
	p := call(t, r, "POST", "/v1/payment-intents", testKey, "create", payload, 201)
	id := p["id"].(string)
	call(t, r, "GET", "/v1/payment-intents/"+id, testKey, "", "", 200)
	call(t, r, "POST", "/v1/payment-intents/"+id+"/confirm", testKey, "", "{}", 200)
	again := call(t, r, "POST", "/v1/payment-intents", testKey, "create", payload, 201)
	if again["status"] != "requires_confirmation" {
		t.Fatal("HTTP replay returned current state")
	}
	call(t, r, "POST", "/v1/payment-intents", testKey, "create", strings.Replace(payload, "2000", "3000", 1), 409)
	call(t, r, "POST", "/v1/payment-intents/"+id+"/capture", testKey, "", "", 409)
	call(t, r, "POST", "/v1/payment-intents/"+id+"/cancel", testKey, "", "", 409)
	ref := call(t, r, "POST", "/v1/refunds", testKey, "refund", `{"payment_intent_id":"`+id+`","amount":500,"metadata":{"order":"sample"}}`, 201)
	rid := ref["id"].(string)
	if _, e = service.SettleRefund(context.Background(), m, rid); e != nil {
		t.Fatal(e)
	}
	call(t, r, "GET", "/v1/refunds/"+rid, testKey, "", "", 200)
	list := call(t, r, "GET", "/v1/refunds?payment_intent_id="+id+"&status=succeeded", testKey, "", "", 200)
	if len(list["data"].([]any)) != 1 {
		t.Fatal(list)
	}
	manual := call(t, r, "POST", "/v1/payment-intents", testKey, "manual", strings.Replace(payload, "automatic", "manual", 1), 201)
	mid := manual["id"].(string)
	call(t, r, "POST", "/v1/payment-intents/"+mid+"/confirm", testKey, "", "{}", 200)
	call(t, r, "POST", "/v1/payment-intents/"+mid+"/capture", testKey, "", "", 200)
	canceled := call(t, r, "POST", "/v1/payment-intents", testKey, "cancel", payload, 201)
	call(t, r, "POST", "/v1/payment-intents/"+canceled["id"].(string)+"/cancel", testKey, "", "", 200)
	page := call(t, r, "GET", "/v1/payment-intents?limit=1", testKey, "", "", 200)
	cursor := page["data"].([]any)[0].(map[string]any)["id"].(string)
	call(t, r, "GET", "/v1/payment-intents?starting_after="+cursor+"&limit=1", testKey, "", "", 200)
	call(t, r, "GET", "/v1/payment-intents?starting_after="+cursor+"&ending_before="+id, testKey, "", "", 400)
	other := testRouter(t, service, "other_"+m, 100)
	call(t, other, "GET", "/v1/payment-intents/"+id, testKey, "", "", 404)
	call(t, other, "GET", "/v1/refunds/"+rid, testKey, "", "", 404)
	// The exact int64 value must survive request validation and generated binding.
	max := call(t, r, "POST", "/v1/payment-intents", testKey, "max", strings.Replace(payload, "2000", "9223372036854775807", 1), 201)
	if max["amount"].(json.Number).String() != "9223372036854775807" {
		t.Fatal("int64 precision was lost")
	}
}
