BEGIN;

CREATE TABLE public.payment_requests (
    merchant_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('create_payment_intent', 'create_refund')),
    idempotency_key VARCHAR(255) NOT NULL CHECK (length(idempotency_key) > 0),
    request_hash CHAR(64) NOT NULL CHECK (request_hash::TEXT ~ '^[a-f0-9]{64}$'),
    resource_id TEXT NOT NULL,
    status_code SMALLINT NOT NULL CHECK (status_code = 201),
    response_body JSONB NOT NULL CHECK (jsonb_typeof(response_body) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payment_intent_id TEXT GENERATED ALWAYS AS (
        CASE WHEN operation = 'create_payment_intent' THEN resource_id END
    ) STORED REFERENCES public.payment_intents (id),
    refund_id TEXT GENERATED ALWAYS AS (
        CASE WHEN operation = 'create_refund' THEN resource_id END
    ) STORED REFERENCES public.refunds (id),
    PRIMARY KEY (merchant_id, operation, idempotency_key),
    CHECK ((payment_intent_id IS NULL) <> (refund_id IS NULL))
);

REVOKE ALL ON public.payment_requests FROM PUBLIC, stripe_lite_payments_app;
GRANT SELECT, INSERT ON public.payment_requests TO stripe_lite_payments_app;

COMMIT;
