#!/usr/bin/env bash
set -euo pipefail

command -v curl >/dev/null || { echo 'curl is required for HTTP verification.' >&2; exit 1; }
[[ -x ./bin/server ]] || { echo 'Build the server with make build first.' >&2; exit 1; }
MODE=smoke
VERIFY_API_KEY=''
VERIFY_API_KEYS=''
VERIFY_PROCESSOR=''
if [[ -n "${PAYMENTS_TEST_DATABASE_URL:-}" ]]; then
    MODE=payments
    command -v jq >/dev/null || { echo 'jq is required for Payments verification.' >&2; exit 1; }
    command -v openssl >/dev/null || { echo 'OpenSSL is required for Payments verification.' >&2; exit 1; }
    VERIFY_API_KEY=$(openssl rand -hex 32)
    printf -v VERIFY_API_KEYS '[{"key":"%s","merchant_id":"merchant_demo","scopes":["payment_intents:read","payment_intents:write","refunds:read","refunds:write"]}]' "$VERIFY_API_KEY"
    VERIFY_PROCESSOR=simulated
fi
VERIFY_DIR=$(mktemp -d)
SERVER_PID=''
cleanup() {
    if [[ -n "$SERVER_PID" ]]; then
        kill -TERM "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$VERIFY_DIR"
}
trap cleanup EXIT
fail() {
    echo "Verification failed: $*" >&2
    if [[ -f "$VERIFY_DIR/response" ]]; then cat "$VERIFY_DIR/response" >&2; fi
    if [[ -f "$VERIFY_DIR/server.log" ]]; then cat "$VERIFY_DIR/server.log" >&2; fi
    exit 1
}

VERIFY_PORT=${VERIFY_PORT:-$((20000 + RANDOM % 30000))}
VERIFY_BASE_URL="http://127.0.0.1:$VERIFY_PORT"
# Only the explicit test URL enables DB access; normal shell configuration is isolated.
APP_ENV=development SERVER_HOST=127.0.0.1 SERVER_PORT="$VERIFY_PORT" SERVER_BASE_PATH=/v1 \
    DATABASE_URL="${PAYMENTS_TEST_DATABASE_URL:-}" PAYMENTS_PROCESSOR="$VERIFY_PROCESSOR" \
    PAYMENTS_API_KEYS="$VERIFY_API_KEYS" PAYMENTS_JWT_SECRET='' PAYMENTS_JWT_ISSUER='' PAYMENTS_JWT_AUDIENCE='' \
    PAYMENTS_DEMO_MERCHANT_ID=merchant_demo PAYMENTS_DEMO_CUSTOMER_ID=cus_demo PAYMENTS_RATE_LIMIT=500 \
    ./bin/server >"$VERIFY_DIR/server.log" 2>&1 &
SERVER_PID=$!
READY=0
for ((i=0;i<50;i++)); do
    kill -0 "$SERVER_PID" 2>/dev/null || fail 'The server stopped before it became ready. Check VERIFY_PORT.'
    if curl -fsS --max-time 1 "$VERIFY_BASE_URL/v1/healthz" >"$VERIFY_DIR/health" 2>/dev/null; then READY=1; break; fi
    sleep 0.2
done
[[ "$READY" == 1 ]] || fail 'The server did not become ready.'
kill -0 "$SERVER_PID" 2>/dev/null || fail 'The verification server exited.'
CHECKS=0
CHECK_NAMES=()

# Store responses in the current shell. Smoke assertions need only Bash and curl.
request() {
    local expected=$1 method=$2 path=$3 body=${4:-} idem=${5:-} auth=${6:-yes} status compact token
    local -a args=(-sS --connect-timeout 2 --max-time 5 -X "$method"
        -D "$VERIFY_DIR/headers" -o "$VERIFY_DIR/response" -w '%{http_code}')
    if [[ "$auth" == yes && -n "$VERIFY_API_KEY" ]]; then args+=(-H "X-API-Key: $VERIFY_API_KEY"); fi
    if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' --data-raw "$body"); fi
    if [[ -n "$idem" ]]; then args+=(-H "Idempotency-Key: $idem"); fi
    status=$(curl "${args[@]}" "$VERIFY_BASE_URL$path") || fail "$method $path could not connect."
    RESPONSE=$(cat "$VERIFY_DIR/response")
    LAST_REQUEST="$method $path"
    [[ "$status" == "$expected" ]] || fail "$LAST_REQUEST returned $status; expected $expected."
    if ((expected >= 400)); then
        grep -qi '^Content-Type: application/problem+json' "$VERIFY_DIR/headers" || fail "$LAST_REQUEST has an invalid error format."
        compact=${RESPONSE//[[:space:]]/}
        token="\"status\":$expected"
        [[ "$compact" == *"$token,"* || "$compact" == *"$token}"* ]] || fail "$LAST_REQUEST has an invalid problem status."
        [[ "$compact" == *'"type":"https://api.stripe-lite.internal/errors/'* && "$compact" == *'"title":"'* ]] || fail "$LAST_REQUEST has an invalid problem document."
    fi
    if [[ "$expected" == 401 ]]; then
        grep -qi '^WWW-Authenticate:' "$VERIFY_DIR/headers" || fail 'The 401 response has no authentication challenge.'
    fi
    CHECKS=$((CHECKS + 1))
    CHECK_NAMES+=("$method ${path%%\?*} -> $status")
}
expect_json() {
    jq -e "$1" <<<"$RESPONSE" >/dev/null || fail "$LAST_REQUEST has unexpected JSON."
}
settled() {
    local id=$1 state
    for ((j=0;j<50;j++)); do
        request 200 GET "/v1/refunds/$id"
        state=$(jq -er '.status' <<<"$RESPONSE")
        if [[ "$state" == succeeded ]]; then return; fi
        [[ "$state" == pending ]] || fail "Refund $id failed."
        sleep 0.1
    done
    fail "Refund $id did not settle."
}

request 200 GET /v1/healthz
[[ "$RESPONSE" == *'"status":"ok"'* ]] || fail 'The health probe is not healthy.'
request 308 GET /healthz
grep -qi '^Location: /v1/healthz' "$VERIFY_DIR/headers" || fail 'The health redirect has the wrong prefix.'
request 200 GET /docs
[[ "$RESPONSE" == *swagger-ui* ]] || fail 'The Swagger UI did not load.'
request 200 GET /openapi.json
# Keep large specification checks in Bash to avoid broken pipes with pipefail.
[[ "$RESPONSE" == *'"title": "Stripe-lite API Gateway"'* && "$RESPONSE" == *'"url": "/v1"'* ]] || fail 'The JSON specification has the wrong title or prefix.'
for path in /payment-intents '/payment-intents/{id}' '/payment-intents/{id}/confirm' \
    '/payment-intents/{id}/capture' '/payment-intents/{id}/cancel' /refunds '/refunds/{id}' \
    /webhook-endpoints '/webhook-endpoints/{id}' '/webhook-endpoints/{id}/rotate-secret'; do
    [[ "$RESPONSE" == *"\"$path\":"* ]] || fail "The specification has no $path route."
done
request 200 GET /openapi.yaml
[[ "$RESPONSE" == *'title: Stripe-lite API Gateway'* && "$RESPONSE" == *'openapi: 3.0.3'* ]] || fail 'The YAML specification did not load.'
request 404 GET /v1/not-a-route
request 401 GET /v1/payment-intents '' '' no
request 501 POST /v1/auth/tokens '{"api_key":"sk_test_sample"}'
request 501 GET /v1/webhook-endpoints
request 501 POST /v1/webhook-endpoints '{"url":"https://example.com/webhook","events":["payment_intent.succeeded"]}'
request 501 GET /v1/webhook-endpoints/we_smoke
request 501 PATCH /v1/webhook-endpoints/we_smoke '{"url":"https://example.com/webhook-updated"}'
request 501 DELETE /v1/webhook-endpoints/we_smoke
request 501 POST /v1/webhook-endpoints/we_smoke/rotate-secret

if [[ "$MODE" == payments ]]; then
    BODY='{"amount":2000,"currency":"usd","customer_id":"cus_demo","capture_method":"automatic","payment_method_id":"pm_demo"}'
    request 400 POST /v1/payment-intents "$BODY"
    request 201 POST /v1/payment-intents "$BODY" create-auto
    PAYMENT_ID=$(jq -er '.id' <<<"$RESPONSE")
    PAYMENT_JSON=$(jq -cS . <<<"$RESPONSE")
    request 200 GET "/v1/payment-intents/$PAYMENT_ID"
    [[ $(jq -cS . <<<"$RESPONSE") == "$PAYMENT_JSON" ]] || fail 'Retrieval differs from the creation response.'
    request 200 POST "/v1/payment-intents/$PAYMENT_ID/confirm" '{}'
    expect_json '.status == "succeeded"'
    request 201 POST /v1/payment-intents "$BODY" create-auto
    [[ $(jq -cS . <<<"$RESPONSE") == "$PAYMENT_JSON" ]] || fail 'Idempotent replay changed the original response.'
    request 409 POST /v1/payment-intents "${BODY/2000/1000}" create-auto
    request 409 POST "/v1/payment-intents/$PAYMENT_ID/capture"
    request 409 POST "/v1/payment-intents/$PAYMENT_ID/cancel"
    printf -v REFUND_BODY '{"payment_intent_id":"%s","amount":500,"metadata":{"order":"example"}}' "$PAYMENT_ID"
    request 201 POST /v1/refunds "$REFUND_BODY" refund-partial
    expect_json '.status == "pending" and .amount == 500'
    REFUND_ID=$(jq -er '.id' <<<"$RESPONSE")
    REFUND_JSON=$(jq -cS . <<<"$RESPONSE")
    settled "$REFUND_ID"
    request 201 POST /v1/refunds "$REFUND_BODY" refund-partial
    [[ $(jq -cS . <<<"$RESPONSE") == "$REFUND_JSON" ]] || fail 'Refund replay changed the original response.'
    printf -v FULL_BODY '{"payment_intent_id":"%s"}' "$PAYMENT_ID"
    request 201 POST /v1/refunds "$FULL_BODY" refund-full
    expect_json '.amount == 1500'
    FULL_ID=$(jq -er '.id' <<<"$RESPONSE")
    settled "$FULL_ID"
    request 200 GET "/v1/payment-intents/$PAYMENT_ID"
    expect_json '.amount_refunded == 2000 and .amount_refund_pending == 0'
    printf -v EXCESS_BODY '{"payment_intent_id":"%s","amount":1}' "$PAYMENT_ID"
    request 409 POST /v1/refunds "$EXCESS_BODY" refund-excess
    request 201 POST /v1/payment-intents "${BODY/automatic/manual}" manual
    MANUAL_ID=$(jq -er '.id' <<<"$RESPONSE")
    request 200 POST "/v1/payment-intents/$MANUAL_ID/confirm" '{}'
    expect_json '.status == "requires_capture"'
    request 200 POST "/v1/payment-intents/$MANUAL_ID/capture"
    expect_json '.status == "succeeded"'
    request 201 POST /v1/payment-intents "$BODY" cancel
    CANCEL_ID=$(jq -er '.id' <<<"$RESPONSE")
    request 200 POST "/v1/payment-intents/$CANCEL_ID/cancel"
    expect_json '.status == "canceled"'
    request 409 POST "/v1/payment-intents/$CANCEL_ID/confirm" '{}'
    request 201 POST /v1/payment-intents "${BODY/pm_demo/pm_declined}" declined
    DECLINED_ID=$(jq -er '.id' <<<"$RESPONSE")
    request 402 POST "/v1/payment-intents/$DECLINED_ID/confirm" '{}'
    request 404 POST /v1/payment-intents "${BODY/cus_demo/cus_other}" other-customer
    request 200 GET '/v1/payment-intents?limit=1'
    expect_json '.object == "list" and (.data | length) == 1 and .has_more'
    CURSOR=$(jq -er '.data[0].id' <<<"$RESPONSE")
    request 200 GET "/v1/payment-intents?limit=1&starting_after=$CURSOR"
    request 200 GET "/v1/refunds?payment_intent_id=$PAYMENT_ID"
    expect_json '(.data | length) == 2'
    request 201 POST /v1/payment-intents "${BODY/2000/9223372036854775807}" int64
    # Inspect the raw JSON so a JSON tool cannot round the int64 boundary.
    grep -Eq '"amount"[[:space:]]*:[[:space:]]*9223372036854775807[[:space:]]*[,}]' "$VERIFY_DIR/response" || fail 'The int64 amount lost precision.'
fi
if [[ -n "${VERIFY_REPORT:-}" ]]; then
    # Check names contain only fixed methods, internal paths, generated IDs and status codes.
    {
        printf '{"passed":true,"mode":"%s","checks":[' "$MODE"
        separator=''
        for check in "${CHECK_NAMES[@]}"; do
            printf '%s"%s"' "$separator" "$check"
            separator=,
        done
        printf ']}\n'
    } >"$VERIFY_REPORT"
fi
echo "Gateway verification passed: $CHECKS HTTP assertions ($MODE mode)."
