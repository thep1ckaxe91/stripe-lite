package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

func (s *Service) CreateIntent(ctx context.Context, m, key string, input CreateIntent) (Intent, error) {
	if input.Amount <= 0 || !validID(input.CustomerID, "cus_") || (input.CaptureMethod != "automatic" && input.CaptureMethod != "manual") {
		return Intent{}, bad("The payment intent fields are invalid.")
	}
	if s == nil || !s.Currencies[input.Currency] {
		return Intent{}, bad("The currency is not supported.")
	}
	if input.PaymentMethodID != nil && !validID(*input.PaymentMethodID, "pm_") {
		return Intent{}, bad("The payment method ID is invalid.")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback(ctx)
	hash, body, err := request(ctx, tx, m, "create_payment_intent", key)
	if err != nil {
		return Intent{}, err
	}
	if body != nil {
		return replay[Intent](hash, body, input)
	}
	if s.Directory == nil {
		return Intent{}, ErrUnavailable
	}
	ok, err := s.Directory.Customer(ctx, tx, m, input.CustomerID)
	if err != nil {
		return Intent{}, err
	}
	if !ok {
		return Intent{}, ErrNotFound
	}
	status := "requires_payment_method"
	if input.PaymentMethodID != nil {
		if err = requireMethod(ctx, s.Directory, tx, m, input.CustomerID, *input.PaymentMethodID); err != nil {
			return Intent{}, err
		}
		status = "requires_confirmation"
	}
	p, err := scanIntent(tx.QueryRow(ctx, `INSERT INTO public.payment_intents(id,merchant_id,customer_id,amount,currency,status,capture_method,payment_method_id,idempotency_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+intentColumns, newID("pi_"), m, input.CustomerID, input.Amount, input.Currency, status, input.CaptureMethod, input.PaymentMethodID, key))
	if err != nil {
		return Intent{}, err
	}
	if err = saveRequest(ctx, tx, m, "create_payment_intent", key, fingerprint(input), p.ID, p); err != nil {
		return Intent{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Intent{}, err
	}
	return p, nil
}

func (s *Service) Confirm(ctx context.Context, m, id string, method *string) (Intent, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback(ctx)
	p, err := getIntent(ctx, tx, m, id, true)
	if err != nil {
		return Intent{}, err
	}
	if p.Status != "requires_payment_method" && p.Status != "requires_confirmation" {
		return Intent{}, ErrConflict
	}
	if method == nil {
		method = p.PaymentMethodID
	}
	if method == nil {
		return Intent{}, bad("A payment method is required.")
	}
	if s.Directory == nil || s.Processor == nil {
		return Intent{}, ErrUnavailable
	}
	if err = requireMethod(ctx, s.Directory, tx, m, p.CustomerID, *method); err != nil {
		return Intent{}, err
	}
	err = s.Processor.Authorize(ctx, id+":authorize:"+*method, *method, p.Amount, p.Currency)
	if errors.Is(err, ErrDeclined) {
		_, err = tx.Exec(ctx, "UPDATE public.payment_intents SET status='requires_payment_method',payment_method_id=NULL WHERE id=$1", id)
		if err != nil {
			return Intent{}, dbError(err)
		}
		if err = commit(ctx, tx); err != nil {
			return Intent{}, err
		}
		return Intent{}, Fail(402, "payment-declined", "Authorization was declined. Supply another payment method.")
	}
	if err != nil {
		return Intent{}, processorError(err)
	}
	if p.CaptureMethod == "automatic" {
		if err = postLedger(ctx, tx, m, p.Currency, id, p.Amount, false); err != nil {
			return Intent{}, err
		}
		if err = s.Processor.Capture(ctx, id+":capture", *method, p.Amount, p.Currency); err != nil {
			return Intent{}, processorError(err)
		}
		p, err = scanIntent(tx.QueryRow(ctx, "UPDATE public.payment_intents SET status='succeeded',payment_method_id=$2,amount_received=amount WHERE id=$1 RETURNING "+intentColumns, id, *method))
	} else {
		p, err = scanIntent(tx.QueryRow(ctx, "UPDATE public.payment_intents SET status='requires_capture',payment_method_id=$2,amount_capturable=amount WHERE id=$1 RETURNING "+intentColumns, id, *method))
	}
	if err != nil {
		return Intent{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Intent{}, err
	}
	return p, nil
}
func processorError(err error) error {
	if errors.Is(err, ErrUnavailable) {
		return ErrUnavailable
	}
	return internal(err)
}
func (s *Service) Capture(ctx context.Context, m, id string) (Intent, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback(ctx)
	p, err := getIntent(ctx, tx, m, id, true)
	if err != nil {
		return Intent{}, err
	}
	if p.Status != "requires_capture" || p.CaptureMethod != "manual" {
		return Intent{}, ErrConflict
	}
	if s.Processor == nil {
		return Intent{}, ErrUnavailable
	}
	if err = postLedger(ctx, tx, m, p.Currency, id, p.Amount, false); err != nil {
		return Intent{}, err
	}
	if err = s.Processor.Capture(ctx, id+":capture", *p.PaymentMethodID, p.Amount, p.Currency); err != nil {
		return Intent{}, processorError(err)
	}
	p, err = scanIntent(tx.QueryRow(ctx, "UPDATE public.payment_intents SET status='succeeded',amount_received=amount,amount_capturable=0 WHERE id=$1 RETURNING "+intentColumns, id))
	if err != nil {
		return Intent{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Intent{}, err
	}
	return p, nil
}
func (s *Service) Cancel(ctx context.Context, m, id string) (Intent, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Intent{}, err
	}
	defer tx.Rollback(ctx)
	p, err := getIntent(ctx, tx, m, id, true)
	if err != nil {
		return Intent{}, err
	}
	if p.Status == "canceled" {
		return p, nil
	}
	if p.Status == "succeeded" {
		return Intent{}, ErrConflict
	}
	if p.Status == "requires_capture" {
		if s.Processor == nil {
			return Intent{}, ErrUnavailable
		}
		if err = s.Processor.Cancel(ctx, id+":cancel", *p.PaymentMethodID, p.Amount, p.Currency); err != nil {
			return Intent{}, processorError(err)
		}
	}
	p, err = scanIntent(tx.QueryRow(ctx, "UPDATE public.payment_intents SET status='canceled',amount_capturable=0 WHERE id=$1 RETURNING "+intentColumns, id))
	if err != nil {
		return Intent{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Intent{}, err
	}
	return p, nil
}
func (s *Service) CreateRefund(ctx context.Context, m, key string, input CreateRefund) (Refund, error) {
	if !validID(input.PaymentIntentID, "pi_") {
		return Refund{}, bad("The payment intent ID is invalid.")
	}
	if input.Amount != nil && *input.Amount <= 0 {
		return Refund{}, bad("The refund amount must be positive.")
	}
	if input.Reason != nil && *input.Reason != "duplicate" && *input.Reason != "fraudulent" && *input.Reason != "requested_by_customer" {
		return Refund{}, bad("The refund reason is invalid.")
	}
	if input.Metadata == nil {
		input.Metadata = map[string]string{}
	}
	if len(input.Metadata) > 50 {
		return Refund{}, bad("Metadata cannot have more than 50 entries.")
	}
	for _, value := range input.Metadata {
		if utf8.RuneCountInString(value) > 500 {
			return Refund{}, bad("A metadata value exceeds 500 characters.")
		}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Refund{}, err
	}
	defer tx.Rollback(ctx)
	hash, body, err := request(ctx, tx, m, "create_refund", key)
	if err != nil {
		return Refund{}, err
	}
	if body != nil {
		var original Refund
		if err = json.Unmarshal(body, &original); err != nil {
			return Refund{}, internal(err)
		}
		if input.Amount == nil {
			input.Amount = &original.Amount
		}
		return replay[Refund](hash, body, input)
	}
	p, err := getIntent(ctx, tx, m, input.PaymentIntentID, true)
	if err != nil {
		return Refund{}, err
	}
	if p.Status != "succeeded" {
		return Refund{}, ErrConflict
	}
	available := p.AmountReceived - p.AmountRefunded - p.AmountRefundPending
	if input.Amount == nil {
		input.Amount = &available
	}
	if *input.Amount <= 0 || *input.Amount > available {
		return Refund{}, Fail(409, "refund-exceeds-available", "The refund exceeds the captured amount that is available.")
	}
	r, err := scanRefund(tx.QueryRow(ctx, `INSERT INTO public.refunds(id,payment_intent_id,amount,reason,metadata,idempotency_key) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+refundColumns, newID("re_"), input.PaymentIntentID, *input.Amount, input.Reason, input.Metadata, key))
	if err != nil {
		return Refund{}, err
	}
	if err = saveRequest(ctx, tx, m, "create_refund", key, fingerprint(input), r.ID, r); err != nil {
		return Refund{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Refund{}, err
	}
	return r, nil
}

// SettleRefund is an internal operation. No public endpoint can force settlement.
func (s *Service) SettleRefund(ctx context.Context, m, id string) (Refund, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Refund{}, err
	}
	defer tx.Rollback(ctx)
	original, err := getRefund(ctx, tx, m, id, false)
	if err != nil {
		return Refund{}, err
	}
	p, err := getIntent(ctx, tx, m, original.PaymentIntentID, true)
	if err != nil {
		return Refund{}, err
	}
	r, err := getRefund(ctx, tx, m, id, true)
	if err != nil {
		return Refund{}, err
	}
	if r.Status != "pending" {
		return r, nil
	}
	if s.Processor == nil {
		return Refund{}, ErrUnavailable
	}
	err = s.Processor.Refund(ctx, id+":refund", *p.PaymentMethodID, r.Amount, r.Currency)
	status := "succeeded"
	if errors.Is(err, ErrDeclined) {
		status = "failed"
	} else if err != nil {
		return Refund{}, processorError(err)
	}
	if status == "succeeded" {
		if err = postLedger(ctx, tx, m, r.Currency, r.ID, r.Amount, true); err != nil {
			return Refund{}, err
		}
	}
	r, err = scanRefund(tx.QueryRow(ctx, "UPDATE public.refunds SET status=$2 WHERE id=$1 RETURNING "+refundColumns, id, status))
	if err != nil {
		return Refund{}, err
	}
	if err = commit(ctx, tx); err != nil {
		return Refund{}, err
	}
	return r, nil
}
func (s *Service) SettlePending(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return ErrUnavailable
	}
	rows, err := s.DB.Query(ctx, "SELECT merchant_id,id FROM public.refunds WHERE status='pending' ORDER BY created_at,id LIMIT 100")
	if err != nil {
		return dbError(err)
	}
	var work [][2]string
	for rows.Next() {
		var m, id string
		if err = rows.Scan(&m, &id); err != nil {
			rows.Close()
			return dbError(err)
		}
		work = append(work, [2]string{m, id})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return dbError(err)
	}
	for _, item := range work {
		if _, err = s.SettleRefund(ctx, item[0], item[1]); err != nil && !errors.Is(err, ErrUnavailable) {
			return err
		}
	}
	return nil
}
func (s *Service) list(ctx context.Context, m string, p PageQuery, refund bool) ([]Intent, []Refund, bool, error) {
	if err := checkPage(p); err != nil {
		return nil, nil, false, err
	}
	if p.Limit == 0 {
		p.Limit = 10
	}
	if p.CustomerID != "" && !validID(p.CustomerID, "cus_") || p.PaymentIntentID != "" && !validID(p.PaymentIntentID, "pi_") {
		return nil, nil, false, bad("The resource filter is invalid.")
	}
	if p.Status != "" {
		valid := map[string]bool{"requires_payment_method": true, "requires_confirmation": true, "requires_capture": true, "succeeded": true, "canceled": true}
		if refund {
			valid = map[string]bool{"pending": true, "succeeded": true, "failed": true}
		}
		if !valid[p.Status] {
			return nil, nil, false, bad("The status filter is invalid.")
		}
	}
	if s == nil || s.DB == nil {
		return nil, nil, false, ErrUnavailable
	}
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, false, dbError(err)
	}
	defer tx.Rollback(ctx)
	table, columns, prefix := "payment_intents", intentColumns, "pi_"
	if refund {
		table, columns, prefix = "refunds", refundColumns, "re_"
	}
	condition, args := where(m, p, refund)
	cursor := p.StartingAfter
	if cursor == "" {
		cursor = p.EndingBefore
	}
	if cursor != "" {
		if !validID(cursor, prefix) {
			return nil, nil, false, bad("The cursor is invalid for this collection.")
		}
		var date any
		var id string
		err = tx.QueryRow(ctx, fmt.Sprintf("SELECT created_at,id FROM public.%s WHERE %s AND id=$%d", table, condition, len(args)+1), append(append([]any{}, args...), cursor)...).Scan(&date, &id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, false, bad("The cursor is not available in this filtered collection.")
		}
		if err != nil {
			return nil, nil, false, dbError(err)
		}
		op := "<"
		if p.EndingBefore != "" {
			op = ">"
		}
		condition += fmt.Sprintf(" AND (created_at,id) %s ($%d,$%d)", op, len(args)+1, len(args)+2)
		args = append(args, date, id)
	}
	direction := "DESC"
	if p.EndingBefore != "" {
		direction = "ASC"
	}
	args = append(args, p.Limit+1)
	rows, err := tx.Query(ctx, fmt.Sprintf("SELECT %s FROM public.%s WHERE %s ORDER BY created_at %s,id %s LIMIT $%d", columns, table, condition, direction, direction, len(args)), args...)
	if err != nil {
		return nil, nil, false, dbError(err)
	}
	defer rows.Close()
	intents, refunds := []Intent{}, []Refund{}
	for rows.Next() {
		if refund {
			value, e := scanRefund(rows)
			if e != nil {
				return nil, nil, false, e
			}
			refunds = append(refunds, value)
		} else {
			value, e := scanIntent(rows)
			if e != nil {
				return nil, nil, false, e
			}
			intents = append(intents, value)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, false, dbError(err)
	}
	count := len(intents)
	if refund {
		count = len(refunds)
	}
	more := count > p.Limit
	if more {
		if refund {
			refunds = refunds[:p.Limit]
		} else {
			intents = intents[:p.Limit]
		}
	}
	if p.EndingBefore != "" {
		for i, j := 0, len(intents)-1; i < j; i, j = i+1, j-1 {
			intents[i], intents[j] = intents[j], intents[i]
		}
		for i, j := 0, len(refunds)-1; i < j; i, j = i+1, j-1 {
			refunds[i], refunds[j] = refunds[j], refunds[i]
		}
	}
	return intents, refunds, more, nil
}
func (s *Service) ListIntents(ctx context.Context, m string, p PageQuery) (Page[Intent], error) {
	data, _, more, err := s.list(ctx, m, p, false)
	return Page[Intent]{"list", data, more, "/v1/payment-intents"}, err
}
func (s *Service) ListRefunds(ctx context.Context, m string, p PageQuery) (Page[Refund], error) {
	_, data, more, err := s.list(ctx, m, p, true)
	return Page[Refund]{"list", data, more, "/v1/refunds"}, err
}
