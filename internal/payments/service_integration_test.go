//go:build integration

package payments

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func fixture(t *testing.T) (*Service, string) {
	t.Helper()
	uri := os.Getenv("PAYMENTS_TEST_DATABASE_URL")
	if uri == "" {
		t.Fatal("PAYMENTS_TEST_DATABASE_URL is required; use make test-integration")
	}
	db, err := pgxpool.New(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	m := newID("merchant_")
	return New(db, DemoDirectory{m, "cus_demo"}, SimulatedProcessor{}), m
}
func must[T any](t *testing.T, v T, e error) T {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func ptr[T any](v T) *T { return &v }
func expectStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil || NormalizeError(err).Status != status {
		t.Fatalf("error=%v want status %d", err, status)
	}
}
func create(t *testing.T, s *Service, m, method, capture string, amount int64) Intent {
	t.Helper()
	input := CreateIntent{Amount: amount, Currency: "usd", CustomerID: "cus_demo", CaptureMethod: capture}
	if method != "" {
		input.PaymentMethodID = &method
	}
	v, e := s.CreateIntent(context.Background(), m, newID("key_"), input)
	return must(t, v, e)
}
func confirmed(t *testing.T, s *Service, m, method string, amount int64) Intent {
	t.Helper()
	v := create(t, s, m, method, "automatic", amount)
	r, e := s.Confirm(context.Background(), m, v.ID, nil)
	return must(t, r, e)
}

func TestPaymentLifecycle(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	p := create(t, s, m, "", "automatic", 1000)
	if p.Status != "requires_payment_method" {
		t.Fatal(p.Status)
	}
	_, err := s.Confirm(ctx, m, p.ID, nil)
	expectStatus(t, err, 400)
	p, err = s.Confirm(ctx, m, p.ID, ptr("pm_demo"))
	p = must(t, p, err)
	if p.Status != "succeeded" || p.AmountReceived != 1000 || p.AmountCapturable != 0 {
		t.Fatal(p)
	}
	_, err = s.Capture(ctx, m, p.ID)
	expectStatus(t, err, 409)
	_, err = s.Cancel(ctx, m, p.ID)
	expectStatus(t, err, 409)
	r, err := s.CreateRefund(ctx, m, "refund-partial", CreateRefund{PaymentIntentID: p.ID, Amount: ptr(int64(300))})
	r = must(t, r, err)
	current, e := s.GetIntent(ctx, m, p.ID)
	current = must(t, current, e)
	if r.Status != "pending" || current.AmountRefundPending != 300 || current.AmountRefunded != 0 {
		t.Fatal(current, r)
	}
	r, e = s.SettleRefund(ctx, m, r.ID)
	r = must(t, r, e)
	if r.Status != "succeeded" {
		t.Fatal(r)
	}
	replay, e := s.CreateRefund(ctx, m, "refund-partial", CreateRefund{PaymentIntentID: p.ID, Amount: ptr(int64(300))})
	replay = must(t, replay, e)
	if replay.Status != "pending" || replay.ID != r.ID {
		t.Fatal("refund replay changed after settlement")
	}
	full, e := s.CreateRefund(ctx, m, "refund-full", CreateRefund{PaymentIntentID: p.ID})
	full = must(t, full, e)
	if full.Amount != 700 {
		t.Fatal(full)
	}
	_, e = s.SettleRefund(ctx, m, full.ID)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.CreateRefund(ctx, m, "refund-full", CreateRefund{PaymentIntentID: p.ID})
	again = must(t, again, e)
	if again.ID != full.ID || again.Amount != 700 {
		t.Fatal("full refund replay was re-resolved")
	}
	current, e = s.GetIntent(ctx, m, p.ID)
	current = must(t, current, e)
	if current.Status != "succeeded" || current.AmountRefunded != 1000 || current.AmountRefundPending != 0 {
		t.Fatal(current)
	}
	_, e = s.CreateRefund(ctx, m, "excess", CreateRefund{PaymentIntentID: p.ID, Amount: ptr(int64(1))})
	expectStatus(t, e, 409)
	var count int
	var net string
	e = s.DB.QueryRow(ctx, `SELECT count(*),sum(CASE WHEN entry_type='DEBIT' THEN amount::numeric ELSE -amount::numeric END)::text FROM public.ledger_entries WHERE transaction_id=ANY($1)`, []string{p.ID, r.ID, full.ID}).Scan(&count, &net)
	if e != nil || count != 6 || net != "0" {
		t.Fatalf("ledger count=%d net=%s err=%v", count, net, e)
	}
	_, e = s.GetIntent(ctx, "other-merchant", p.ID)
	expectStatus(t, e, 404)
	_, e = s.GetRefund(ctx, "other-merchant", r.ID)
	expectStatus(t, e, 404)
}

func TestManualDeclineAndRefundFailure(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	p := create(t, s, m, "pm_demo", "manual", 200)
	p, e := s.Confirm(ctx, m, p.ID, nil)
	p = must(t, p, e)
	if p.Status != "requires_capture" || p.AmountCapturable != 200 {
		t.Fatal(p)
	}
	_, e = s.CreateRefund(ctx, m, "early-refund", CreateRefund{PaymentIntentID: p.ID})
	expectStatus(t, e, 409)
	p, e = s.Capture(ctx, m, p.ID)
	p = must(t, p, e)
	if p.Status != "succeeded" {
		t.Fatal(p)
	}
	_, e = s.Capture(ctx, m, p.ID)
	expectStatus(t, e, 409)
	c := create(t, s, m, "pm_demo", "manual", 100)
	_, e = s.Confirm(ctx, m, c.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	c, e = s.Cancel(ctx, m, c.ID)
	c = must(t, c, e)
	if c.Status != "canceled" || c.AmountCapturable != 0 {
		t.Fatal(c)
	}
	c2, e := s.Cancel(ctx, m, c.ID)
	c2 = must(t, c2, e)
	if !reflect.DeepEqual(c, c2) {
		t.Fatal("canceled replay changed")
	}
	_, e = s.Confirm(ctx, m, c.ID, nil)
	expectStatus(t, e, 409)
	d := create(t, s, m, "pm_declined", "automatic", 100)
	_, e = s.Confirm(ctx, m, d.ID, nil)
	expectStatus(t, e, 402)
	d, e = s.GetIntent(ctx, m, d.ID)
	d = must(t, d, e)
	if d.Status != "requires_payment_method" || d.PaymentMethodID != nil {
		t.Fatal(d)
	}
	u := create(t, s, m, "pm_unavailable", "automatic", 100)
	_, e = s.Confirm(ctx, m, u.ID, nil)
	expectStatus(t, e, 503)
	u2, e := s.GetIntent(ctx, m, u.ID)
	u2 = must(t, u2, e)
	if !reflect.DeepEqual(u, u2) {
		t.Fatal("unavailable processor changed state")
	}
	f := confirmed(t, s, m, "pm_refundfailed", 100)
	r, e := s.CreateRefund(ctx, m, "failed-refund", CreateRefund{PaymentIntentID: f.ID})
	r = must(t, r, e)
	r, e = s.SettleRefund(ctx, m, r.ID)
	r = must(t, r, e)
	if r.Status != "failed" {
		t.Fatal(r)
	}
	f, e = s.GetIntent(ctx, m, f.ID)
	f = must(t, f, e)
	if f.AmountRefundPending != 0 || f.AmountRefunded != 0 {
		t.Fatal(f)
	}
	_, e = s.CreateRefund(ctx, m, "retry-new-key", CreateRefund{PaymentIntentID: f.ID})
	if e != nil {
		t.Fatal("failed refund did not release reservation", e)
	}
}

func TestIdempotencyPersistenceAndInProgress(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	input := CreateIntent{100, "usd", "cus_demo", "automatic", ptr("pm_demo")}
	p, e := s.CreateIntent(ctx, m, "original-key", input)
	p = must(t, p, e)
	_, e = s.Confirm(ctx, m, p.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	// A new pool simulates a service restart; replay must use stored response bytes.
	db, e := pgxpool.New(ctx, os.Getenv("PAYMENTS_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	restarted := New(db, nil, nil)
	r, e := restarted.CreateIntent(ctx, m, "original-key", input)
	r = must(t, r, e)
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(r)
	if string(a) != string(b) {
		t.Fatal("response replay changed after restart")
	}
	input.Amount++
	_, e = s.CreateIntent(ctx, m, "original-key", input)
	expectStatus(t, e, 409)
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	_, _, e = request(ctx, tx, m, "create_payment_intent", "busy-key")
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.CreateIntent(ctx, m, "busy-key", input)
	expectStatus(t, e, 409)
	_, e = s.CreateIntent(ctx, m, "bad-customer", CreateIntent{100, "usd", "cus_other", "automatic", nil})
	expectStatus(t, e, 404)
	_, e = s.CreateIntent(ctx, m, "bad-method", CreateIntent{100, "usd", "cus_demo", "automatic", ptr("pm_other")})
	expectStatus(t, e, 404)
}

func TestConcurrentFinancialOperations(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	p := confirmed(t, s, m, "pm_demo", 100)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.CreateRefund(ctx, m, newID("key_"), CreateRefund{PaymentIntentID: p.ID, Amount: ptr(int64(30))})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for e := range results {
		if e == nil {
			succeeded++
		} else {
			expectStatus(t, e, 409)
		}
	}
	if succeeded != 3 {
		t.Fatalf("created %d refunds", succeeded)
	}
	current, e := s.GetIntent(ctx, m, p.ID)
	current = must(t, current, e)
	if current.AmountRefundPending != 90 {
		t.Fatal(current)
	}
	// All calls with one key must refer to one resource, including concurrent retries.
	ids := make(chan string, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.CreateIntent(ctx, m, "same-key", CreateIntent{1, "usd", "cus_demo", "automatic", nil})
			if e != nil {
				errs <- e
			} else {
				ids <- r.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	one := ""
	for id := range ids {
		if one != "" && one != id {
			t.Fatal("duplicate resources for one key")
		}
		one = id
	}
	if one == "" {
		t.Fatal("no create succeeded")
	}
	for e := range errs {
		expectStatus(t, e, 409)
	}
	manual := create(t, s, m, "pm_demo", "manual", 70)
	_, e = s.Confirm(ctx, m, manual.ID, nil)
	if e != nil {
		t.Fatal(e)
	}
	race := make(chan error, 2)
	go func() { _, e := s.Capture(ctx, m, manual.ID); race <- e }()
	go func() { _, e := s.Cancel(ctx, m, manual.ID); race <- e }()
	wins := 0
	for i := 0; i < 2; i++ {
		if e := <-race; e == nil {
			wins++
		} else {
			expectStatus(t, e, 409)
		}
	}
	if wins != 1 {
		t.Fatal("capture/cancel race had multiple winners")
	}
}

func TestCursorPagination(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	for i := 0; i < 5; i++ {
		create(t, s, m, "", "automatic", int64(i+1))
	}
	first, e := s.ListIntents(ctx, m, PageQuery{Limit: 2})
	first = must(t, first, e)
	if len(first.Data) != 2 || !first.HasMore {
		t.Fatal(first)
	}
	second, e := s.ListIntents(ctx, m, PageQuery{Limit: 2, StartingAfter: first.Data[1].ID})
	second = must(t, second, e)
	if len(second.Data) != 2 || !second.HasMore {
		t.Fatal(second)
	}
	prev, e := s.ListIntents(ctx, m, PageQuery{Limit: 2, EndingBefore: second.Data[0].ID})
	prev = must(t, prev, e)
	if !reflect.DeepEqual(first.Data, prev.Data) || prev.HasMore {
		t.Fatal("backward pagination differs")
	}
	last, e := s.ListIntents(ctx, m, PageQuery{Limit: 2, StartingAfter: second.Data[1].ID})
	last = must(t, last, e)
	if len(last.Data) != 1 || last.HasMore {
		t.Fatal(last)
	}
	for _, q := range []PageQuery{{StartingAfter: "re_other"}, {StartingAfter: "pi_missing"}, {StartingAfter: first.Data[0].ID, Status: "canceled"}, {StartingAfter: first.Data[0].ID, EndingBefore: first.Data[1].ID}, {Limit: 101}} {
		_, e = s.ListIntents(ctx, m, q)
		expectStatus(t, e, 400)
	}
	_, e = s.ListIntents(ctx, "other-merchant", PageQuery{StartingAfter: first.Data[0].ID})
	expectStatus(t, e, 400)
	empty, e := s.ListIntents(ctx, m, PageQuery{CustomerID: "cus_other"})
	empty = must(t, empty, e)
	if empty.Data == nil || len(empty.Data) != 0 || empty.HasMore {
		t.Fatal(empty)
	}
}

func TestLedgerLimitsAndDatabaseGuards(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	p := confirmed(t, s, m, "pm_demo", math.MaxInt64)
	next := create(t, s, m, "pm_demo", "automatic", 1)
	_, e := s.Confirm(ctx, m, next.ID, nil)
	expectStatus(t, e, 409)
	current, e := s.GetIntent(ctx, m, next.ID)
	current = must(t, current, e)
	if current.Status != "requires_confirmation" {
		t.Fatal("overflow changed state")
	}
	for _, query := range []string{"UPDATE public.ledger_entries SET amount=amount WHERE transaction_id=$1", "DELETE FROM public.ledger_entries WHERE transaction_id=$1", "UPDATE public.payment_intents SET amount_refunded=1 WHERE id=$1", "DELETE FROM public.payment_requests WHERE resource_id=$1"} {
		if _, e = s.DB.Exec(ctx, query, p.ID); e == nil {
			t.Fatalf("app could execute forbidden mutation: %s", query)
		}
	}
	adminURL := os.Getenv("PAYMENTS_TEST_ADMIN_URL")
	if adminURL == "" {
		t.Fatal("admin test URL is required for database guard tests")
	}
	admin, e := pgxpool.New(ctx, adminURL)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	// Owner privileges cannot bypass the append-only trigger.
	for _, query := range []string{"UPDATE public.ledger_entries SET amount=amount WHERE transaction_id=$1", "DELETE FROM public.ledger_entries WHERE transaction_id=$1"} {
		if _, e = admin.Exec(ctx, query, p.ID); e == nil {
			t.Fatal("ledger mutation succeeded")
		}
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	raw := newID("pi_")
	_, e = tx.Exec(ctx, `INSERT INTO public.payment_intents(id,merchant_id,customer_id,amount,currency,status,capture_method,payment_method_id,idempotency_key) VALUES($1::text,$2,'cus_demo',10,'usd','requires_confirmation','automatic','pm_demo',$1::text)`, raw, m)
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(ctx, `UPDATE public.payment_intents SET status='succeeded',amount_received=amount WHERE id=$1`, raw)
	if e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e == nil {
		t.Fatal("captured payment without ledger committed")
	}
	_, e = s.GetIntent(ctx, m, raw)
	expectStatus(t, e, 404)
}

func TestIdentityDirectory(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t)
	admin, e := pgxpool.New(ctx, os.Getenv("PAYMENTS_TEST_ADMIN_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close()
	// These are adapter fixtures, not Identity migrations or application-owned tables.
	_, e = admin.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.customers(id text PRIMARY KEY,merchant_id text NOT NULL);
 CREATE TABLE IF NOT EXISTS public.payment_methods(id text PRIMARY KEY,merchant_id text NOT NULL,customer_id text NOT NULL);
 GRANT SELECT ON public.customers,public.payment_methods TO stripe_lite_payments_app;`)
	if e != nil {
		t.Fatal(e)
	}
	customer, method := newID("cus_"), newID("pm_")
	_, e = admin.Exec(ctx, "INSERT INTO public.customers VALUES($1,$2)", customer, m)
	if e != nil {
		t.Fatal(e)
	}
	_, e = admin.Exec(ctx, "INSERT INTO public.payment_methods VALUES($1,$2,$3)", method, m, customer)
	if e != nil {
		t.Fatal(e)
	}
	service := New(s.DB, SQLDirectory{}, nil)
	input := CreateIntent{10, "usd", customer, "automatic", &method}
	p, e := service.CreateIntent(ctx, m, "owned", input)
	p = must(t, p, e)
	_, e = service.Confirm(ctx, m, p.ID, nil)
	expectStatus(t, e, 503)
	_, e = service.CreateIntent(ctx, "other-merchant", "foreign", input)
	expectStatus(t, e, 404)
	input.PaymentMethodID = ptr("pm_missing")
	_, e = service.CreateIntent(ctx, m, "wrong-method", input)
	expectStatus(t, e, 404)
}
