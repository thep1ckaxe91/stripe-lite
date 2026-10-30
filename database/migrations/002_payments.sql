BEGIN;

CREATE TABLE public.payment_intents (
    id TEXT PRIMARY KEY CHECK (id ~ '^pi_[A-Za-z0-9]+$' AND length(id) <= 255),
    merchant_id TEXT NOT NULL CHECK (length(btrim(merchant_id)) BETWEEN 1 AND 255),
    customer_id TEXT NOT NULL CHECK (customer_id ~ '^cus_[A-Za-z0-9]+$' AND length(customer_id) <= 255),
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency CHAR(3) NOT NULL CHECK (currency::TEXT ~ '^[a-z]{3}$'),
    status TEXT NOT NULL DEFAULT 'requires_payment_method'
        CHECK (status IN ('requires_payment_method', 'requires_confirmation', 'requires_capture', 'succeeded', 'canceled')),
    capture_method TEXT NOT NULL CHECK (capture_method IN ('automatic', 'manual')),
    payment_method_id TEXT CHECK (payment_method_id ~ '^pm_[A-Za-z0-9]+$' AND length(payment_method_id) <= 255),
    idempotency_key VARCHAR(255) NOT NULL CHECK (length(idempotency_key) > 0),
    amount_capturable BIGINT NOT NULL DEFAULT 0 CHECK (amount_capturable >= 0),
    amount_received BIGINT NOT NULL DEFAULT 0 CHECK (amount_received >= 0),
    amount_refunded BIGINT NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0),
    amount_refund_pending BIGINT NOT NULL DEFAULT 0 CHECK (amount_refund_pending >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT payment_intents_idempotency UNIQUE (merchant_id, idempotency_key),
    CONSTRAINT payment_intents_owner_currency UNIQUE (id, merchant_id, currency),
    CONSTRAINT payment_intents_payment_method CHECK (
        (status = 'requires_payment_method' AND payment_method_id IS NULL)
        OR (status IN ('requires_confirmation', 'requires_capture', 'succeeded') AND payment_method_id IS NOT NULL)
        OR status = 'canceled'
    ),
    CONSTRAINT payment_intents_amounts CHECK (
        (status = 'succeeded' AND amount_received = amount AND amount_capturable = 0)
        OR (status = 'requires_capture' AND capture_method = 'manual'
            AND amount_received = 0 AND amount_capturable = amount)
        OR (status IN ('requires_payment_method', 'requires_confirmation', 'canceled')
            AND amount_received = 0 AND amount_capturable = 0)
    ),
    CONSTRAINT payment_intents_refund_limit CHECK (
        amount_refunded <= amount_received
        AND amount_refund_pending <= amount_received - amount_refunded
    )
);

CREATE INDEX payment_intents_page_idx ON public.payment_intents (merchant_id, created_at DESC, id DESC);
CREATE INDEX payment_intents_customer_page_idx ON public.payment_intents (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX payment_intents_status_page_idx ON public.payment_intents (merchant_id, status, created_at DESC, id DESC);

CREATE TABLE public.refunds (
    id TEXT PRIMARY KEY CHECK (id ~ '^re_[A-Za-z0-9]+$' AND length(id) <= 255),
    payment_intent_id TEXT NOT NULL,
    merchant_id TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency CHAR(3) NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed')),
    reason TEXT CHECK (reason IN ('duplicate', 'fraudulent', 'requested_by_customer')),
    metadata JSONB NOT NULL DEFAULT '{}'::JSONB CHECK (jsonb_typeof(metadata) = 'object'),
    idempotency_key VARCHAR(255) NOT NULL CHECK (length(idempotency_key) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT refunds_payment_owner_currency FOREIGN KEY (payment_intent_id, merchant_id, currency)
        REFERENCES public.payment_intents (id, merchant_id, currency),
    CONSTRAINT refunds_idempotency UNIQUE (merchant_id, idempotency_key)
);

CREATE INDEX refunds_page_idx ON public.refunds (merchant_id, created_at DESC, id DESC);
CREATE INDEX refunds_payment_page_idx ON public.refunds (merchant_id, payment_intent_id, created_at DESC, id DESC);
CREATE INDEX refunds_status_page_idx ON public.refunds (merchant_id, status, created_at DESC, id DESC);

CREATE TABLE public.ledger_entries (
    id TEXT PRIMARY KEY CHECK (length(btrim(id)) BETWEEN 1 AND 255),
    transaction_id TEXT NOT NULL CHECK (transaction_id ~ '^(pi|re)_[A-Za-z0-9]+$'),
    account_id TEXT NOT NULL CHECK (length(btrim(account_id)) BETWEEN 1 AND 255),
    entry_type TEXT NOT NULL CHECK (entry_type IN ('DEBIT', 'CREDIT')),
    amount BIGINT NOT NULL CHECK (amount > 0),
    balance_after BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    -- These columns give transaction_id a real foreign key for either resource type.
    payment_intent_id TEXT GENERATED ALWAYS AS (
        CASE WHEN left(transaction_id, 3) = 'pi_' THEN transaction_id END
    ) STORED REFERENCES public.payment_intents (id),
    refund_id TEXT GENERATED ALWAYS AS (
        CASE WHEN left(transaction_id, 3) = 're_' THEN transaction_id END
    ) STORED REFERENCES public.refunds (id),
    CONSTRAINT ledger_entries_source CHECK ((payment_intent_id IS NULL) <> (refund_id IS NULL)),
    CONSTRAINT ledger_entries_account_side UNIQUE (transaction_id, account_id, entry_type)
);

CREATE INDEX ledger_entries_account_idx ON public.ledger_entries (account_id, created_at, id);

CREATE FUNCTION public.payments_guard_intent() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('requires_payment_method', 'requires_confirmation') THEN
            RAISE EXCEPTION 'A new payment intent must await a payment method or confirmation' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF ROW(NEW.id, NEW.merchant_id, NEW.customer_id, NEW.amount, NEW.currency,
           NEW.capture_method, NEW.idempotency_key, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.merchant_id, OLD.customer_id, OLD.amount, OLD.currency,
           OLD.capture_method, OLD.idempotency_key, OLD.created_at) THEN
        RAISE EXCEPTION 'Payment intent identity and financial terms cannot change' USING ERRCODE = '23514';
    END IF;
    IF OLD.status IN ('requires_capture', 'succeeded', 'canceled')
       AND NEW.payment_method_id IS DISTINCT FROM OLD.payment_method_id THEN
        RAISE EXCEPTION 'The payment method cannot change after authorization' USING ERRCODE = '23514';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'requires_payment_method' AND NEW.status IN ('requires_confirmation', 'requires_capture', 'succeeded', 'canceled'))
        OR (OLD.status = 'requires_confirmation' AND NEW.status IN ('requires_payment_method', 'requires_capture', 'succeeded', 'canceled'))
        OR (OLD.status = 'requires_capture' AND NEW.status IN ('succeeded', 'canceled'))
    ) THEN
        RAISE EXCEPTION 'Invalid payment intent state transition' USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'succeeded' AND OLD.status IN ('requires_payment_method', 'requires_confirmation')
       AND NEW.capture_method <> 'automatic' THEN
        RAISE EXCEPTION 'Manual authorization must precede capture' USING ERRCODE = '23514';
    END IF;
    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_guard_intent
BEFORE INSERT OR UPDATE ON public.payment_intents
FOR EACH ROW EXECUTE FUNCTION public.payments_guard_intent();

CREATE FUNCTION public.payments_guard_refund() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
    payment public.payment_intents%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'Refund records cannot be deleted' USING ERRCODE = '55000';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF ROW(NEW.id, NEW.payment_intent_id, NEW.merchant_id, NEW.amount, NEW.currency,
               NEW.reason, NEW.metadata, NEW.idempotency_key, NEW.created_at)
           IS DISTINCT FROM
           ROW(OLD.id, OLD.payment_intent_id, OLD.merchant_id, OLD.amount, OLD.currency,
               OLD.reason, OLD.metadata, OLD.idempotency_key, OLD.created_at) THEN
            RAISE EXCEPTION 'Refund identity and financial terms cannot change' USING ERRCODE = '23514';
        END IF;
        IF NEW.status IS DISTINCT FROM OLD.status
           AND NOT (OLD.status = 'pending' AND NEW.status IN ('succeeded', 'failed')) THEN
            RAISE EXCEPTION 'Invalid refund state transition' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.status <> 'pending' THEN
        RAISE EXCEPTION 'A new refund must be pending' USING ERRCODE = '23514';
    END IF;

    -- The parent row serializes reservations for concurrent refund requests.
    SELECT * INTO payment FROM public.payment_intents WHERE id = NEW.payment_intent_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'The payment intent does not exist' USING ERRCODE = '23503';
    END IF;
    IF payment.status <> 'succeeded' THEN
        RAISE EXCEPTION 'A refund requires a captured payment' USING ERRCODE = '23514';
    END IF;
    IF NEW.merchant_id IS NOT NULL AND NEW.merchant_id <> payment.merchant_id THEN
        RAISE EXCEPTION 'The refund merchant does not own the payment' USING ERRCODE = '23514';
    END IF;
    IF NEW.currency IS NOT NULL AND NEW.currency <> payment.currency THEN
        RAISE EXCEPTION 'The refund currency must match the payment' USING ERRCODE = '23514';
    END IF;
    NEW.merchant_id := payment.merchant_id;
    NEW.currency := payment.currency;

    IF TG_OP = 'INSERT' THEN
        -- A duplicate key reaches the unique constraint without another reservation.
        IF EXISTS (SELECT 1 FROM public.refunds
                   WHERE merchant_id = NEW.merchant_id AND idempotency_key = NEW.idempotency_key) THEN
            RETURN NEW;
        END IF;
        IF NEW.amount > payment.amount_received - payment.amount_refunded - payment.amount_refund_pending THEN
            RAISE EXCEPTION 'The refund exceeds the available captured amount' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_guard_refund
BEFORE INSERT OR UPDATE OR DELETE ON public.refunds
FOR EACH ROW EXECUTE FUNCTION public.payments_guard_refund();

CREATE FUNCTION public.payments_update_refund_totals() RETURNS TRIGGER
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    pending_delta BIGINT := 0;
    refunded_delta BIGINT := 0;
BEGIN
    IF TG_OP = 'INSERT' THEN
        pending_delta := NEW.amount;
    ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
        pending_delta := -OLD.amount;
        IF NEW.status = 'succeeded' THEN
            refunded_delta := NEW.amount;
        END IF;
    END IF;
    IF pending_delta <> 0 OR refunded_delta <> 0 THEN
        UPDATE public.payment_intents
        SET amount_refund_pending = amount_refund_pending + pending_delta,
            amount_refunded = amount_refunded + refunded_delta
        WHERE id = NEW.payment_intent_id;
    END IF;
    RETURN NULL;
END;
$$;

-- AFTER INSERT avoids counter changes for INSERT ... ON CONFLICT DO NOTHING.
CREATE TRIGGER payments_update_refund_totals
AFTER INSERT OR UPDATE OF status ON public.refunds
FOR EACH ROW EXECUTE FUNCTION public.payments_update_refund_totals();

CREATE FUNCTION public.payments_lock_ledger_source() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    IF left(NEW.transaction_id, 3) = 'pi_' THEN
        PERFORM 1 FROM public.payment_intents WHERE id = NEW.transaction_id FOR UPDATE;
    ELSE
        PERFORM 1 FROM public.refunds WHERE id = NEW.transaction_id FOR UPDATE;
    END IF;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'The ledger source does not exist' USING ERRCODE = '23503';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_lock_ledger_source
BEFORE INSERT ON public.ledger_entries
FOR EACH ROW EXECUTE FUNCTION public.payments_lock_ledger_source();

CREATE FUNCTION public.payments_reject_ledger_change() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
BEGIN
    RAISE EXCEPTION 'Ledger entries are append-only' USING ERRCODE = '55000';
END;
$$;

CREATE TRIGGER payments_reject_ledger_change
BEFORE UPDATE OR DELETE OR TRUNCATE ON public.ledger_entries
FOR EACH STATEMENT EXECUTE FUNCTION public.payments_reject_ledger_change();

CREATE FUNCTION public.payments_check_ledger() RETURNS TRIGGER
LANGUAGE plpgsql SET search_path = pg_catalog, public AS $$
DECLARE
    source_id TEXT;
    source_amount BIGINT;
    source_status TEXT;
    debit_total NUMERIC;
    credit_total NUMERIC;
    account_count BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'ledger_entries' THEN
        source_id := NEW.transaction_id;
    ELSE
        source_id := NEW.id;
    END IF;
    IF left(source_id, 3) = 'pi_' THEN
        SELECT amount, status INTO source_amount, source_status
        FROM public.payment_intents WHERE id = source_id;
    ELSE
        SELECT amount, status INTO source_amount, source_status
        FROM public.refunds WHERE id = source_id;
    END IF;
    SELECT COALESCE(sum(amount) FILTER (WHERE entry_type = 'DEBIT'), 0),
           COALESCE(sum(amount) FILTER (WHERE entry_type = 'CREDIT'), 0),
           count(DISTINCT account_id)
    INTO debit_total, credit_total, account_count
    FROM public.ledger_entries WHERE transaction_id = source_id;

    IF source_status <> 'succeeded' OR debit_total <> source_amount
       OR credit_total <> source_amount OR account_count < 2 THEN
        RAISE EXCEPTION 'A completed transaction needs balanced ledger entries for its full amount'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

-- Check all rows at commit so a debit and its credits can be inserted separately.
CREATE CONSTRAINT TRIGGER payments_ledger_balance
AFTER INSERT ON public.ledger_entries DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION public.payments_check_ledger();

CREATE CONSTRAINT TRIGGER payments_capture_ledger
AFTER UPDATE OF status ON public.payment_intents DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW WHEN (NEW.status = 'succeeded' AND OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION public.payments_check_ledger();

CREATE CONSTRAINT TRIGGER payments_refund_ledger
AFTER UPDATE OF status ON public.refunds DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW WHEN (NEW.status = 'succeeded' AND OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION public.payments_check_ledger();

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'stripe_lite_payments_app') THEN
        CREATE ROLE stripe_lite_payments_app NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
    END IF;
END;
$$;

REVOKE ALL ON public.payment_intents, public.refunds, public.ledger_entries FROM PUBLIC;
REVOKE ALL ON public.payment_intents, public.refunds, public.ledger_entries FROM stripe_lite_payments_app;
GRANT USAGE ON SCHEMA public TO stripe_lite_payments_app;
GRANT SELECT, INSERT ON public.payment_intents, public.refunds, public.ledger_entries TO stripe_lite_payments_app;
GRANT UPDATE (status, payment_method_id, amount_capturable, amount_received)
    ON public.payment_intents TO stripe_lite_payments_app;
GRANT UPDATE (status) ON public.refunds TO stripe_lite_payments_app;

REVOKE ALL ON FUNCTION public.payments_guard_intent(), public.payments_guard_refund(),
    public.payments_update_refund_totals(), public.payments_lock_ledger_source(),
    public.payments_reject_ledger_change(), public.payments_check_ledger() FROM PUBLIC;

COMMIT;
