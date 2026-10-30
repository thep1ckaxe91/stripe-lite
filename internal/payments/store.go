package payments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	DB         *pgxpool.Pool
	Directory  Directory
	Processor  Processor
	Currencies map[string]bool
}

func New(db *pgxpool.Pool, d Directory, p Processor) *Service {
	return &Service{DB: db, Directory: d, Processor: p, Currencies: map[string]bool{"usd": true, "eur": true, "vnd": true}}
}

const intentColumns = `id,customer_id,payment_method_id,amount,currency,status,capture_method,amount_capturable,amount_received,amount_refunded,amount_refund_pending,created_at,updated_at`
const refundColumns = `id,payment_intent_id,amount,currency,status,reason,metadata,created_at`

func scanIntent(row pgx.Row) (Intent, error) {
	p := Intent{Object: "payment_intent"}
	err := row.Scan(&p.ID, &p.CustomerID, &p.PaymentMethodID, &p.Amount, &p.Currency, &p.Status, &p.CaptureMethod, &p.AmountCapturable, &p.AmountReceived, &p.AmountRefunded, &p.AmountRefundPending, &p.CreatedAt, &p.UpdatedAt)
	p.CreatedAt = p.CreatedAt.UTC()
	p.UpdatedAt = p.UpdatedAt.UTC()
	return p, dbError(err)
}
func scanRefund(row pgx.Row) (Refund, error) {
	r := Refund{Object: "refund"}
	err := row.Scan(&r.ID, &r.PaymentIntentID, &r.Amount, &r.Currency, &r.Status, &r.Reason, &r.Metadata, &r.CreatedAt)
	r.CreatedAt = r.CreatedAt.UTC()
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	return r, dbError(err)
}
func dbError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return Fail(409, "idempotency-conflict", "A resource with this key already exists.")
		case "23503":
			return ErrNotFound
		case "23514", "22003":
			return ErrConflict
		case "40001", "40P01":
			return Fail(409, "request-in-progress", "The resource is busy. Retry the request.")
		}
	}
	return internal(err)
}
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *Service) begin(ctx context.Context) (pgx.Tx, error) {
	if s == nil || s.DB == nil {
		return nil, ErrUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	return tx, dbError(err)
}
func getIntent(ctx context.Context, q Querier, m, id string, lock bool) (Intent, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	return scanIntent(q.QueryRow(ctx, "SELECT "+intentColumns+" FROM public.payment_intents WHERE merchant_id=$1 AND id=$2"+suffix, m, id))
}
func getRefund(ctx context.Context, q Querier, m, id string, lock bool) (Refund, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	return scanRefund(q.QueryRow(ctx, "SELECT "+refundColumns+" FROM public.refunds WHERE merchant_id=$1 AND id=$2"+suffix, m, id))
}
func (s *Service) GetIntent(ctx context.Context, m, id string) (Intent, error) {
	if !validID(id, "pi_") {
		return Intent{}, bad("The payment intent ID is invalid.")
	}
	if s == nil || s.DB == nil {
		return Intent{}, ErrUnavailable
	}
	return getIntent(ctx, s.DB, m, id, false)
}
func (s *Service) GetRefund(ctx context.Context, m, id string) (Refund, error) {
	if !validID(id, "re_") {
		return Refund{}, bad("The refund ID is invalid.")
	}
	if s == nil || s.DB == nil {
		return Refund{}, ErrUnavailable
	}
	return getRefund(ctx, s.DB, m, id, false)
}
func request(ctx context.Context, tx pgx.Tx, m, op, key string) (hash string, body []byte, err error) {
	if len(key) < 1 || len(key) > 255 {
		return "", nil, bad("Idempotency-Key must contain 1 to 255 characters.")
	}
	var locked bool
	err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))", m+"\x1f"+op+"\x1f"+key).Scan(&locked)
	if err != nil {
		return "", nil, dbError(err)
	}
	if !locked {
		return "", nil, Fail(409, "request-in-progress", "A request with this key is still in progress.")
	}
	err = tx.QueryRow(ctx, "SELECT request_hash,response_body FROM public.payment_requests WHERE merchant_id=$1 AND operation=$2 AND idempotency_key=$3", m, op, key).Scan(&hash, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return hash, body, dbError(err)
}
func saveRequest(ctx context.Context, tx pgx.Tx, m, op, key, hash, id string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return internal(err)
	}
	_, err = tx.Exec(ctx, "INSERT INTO public.payment_requests(merchant_id,operation,idempotency_key,request_hash,resource_id,status_code,response_body) VALUES($1,$2,$3,$4,$5,201,$6)", m, op, key, hash, id, body)
	return dbError(err)
}
func replay[T any](hash string, body []byte, input any) (T, error) {
	var v T
	if hash != fingerprint(input) {
		return v, Fail(409, "idempotency-conflict", "This key belongs to a different request payload.")
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return v, internal(err)
	}
	return v, nil
}
func commit(ctx context.Context, tx pgx.Tx) error { return dbError(tx.Commit(ctx)) }

// postLedger locks accounts in a fixed order and computes balances from immutable entries.
func postLedger(ctx context.Context, tx pgx.Tx, merchant, currency, source string, amount int64, refund bool) error {
	account := func(kind string) string {
		h := sha256.Sum256([]byte(merchant + "|" + currency + "|" + kind))
		return "acct_" + hex.EncodeToString(h[:])
	}
	clearing, merchantAccount := account("clearing"), account("merchant")
	ids := []string{clearing, merchantAccount}
	if ids[0] > ids[1] {
		ids[0], ids[1] = ids[1], ids[0]
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,1))", id); err != nil {
			return dbError(err)
		}
	}
	for _, id := range []string{clearing, merchantAccount} {
		var current string
		err := tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN entry_type='CREDIT' THEN amount::numeric ELSE -amount::numeric END),0)::text FROM public.ledger_entries WHERE account_id=$1`, id).Scan(&current)
		if err != nil {
			return dbError(err)
		}
		balance, ok := new(big.Int).SetString(current, 10)
		if !ok {
			return internal(fmt.Errorf("invalid ledger balance"))
		}
		side := "DEBIT"
		if id == merchantAccount {
			side = "CREDIT"
		}
		if refund {
			if side == "DEBIT" {
				side = "CREDIT"
			} else {
				side = "DEBIT"
			}
		}
		delta := big.NewInt(amount)
		if side == "DEBIT" {
			delta.Neg(delta)
		}
		balance.Add(balance, delta)
		if !balance.IsInt64() {
			return Fail(409, "balance-limit", "The ledger balance exceeds the supported range.")
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.ledger_entries(id,transaction_id,account_id,entry_type,amount,balance_after) VALUES($1,$2,$3,$4,$5,$6)", newID("le_"), source, id, side, amount, balance.Int64())
		if err != nil {
			return dbError(err)
		}
	}
	return nil
}
func where(m string, p PageQuery, refund bool) (string, []any) {
	args := []any{m}
	parts := []string{"merchant_id=$1"}
	add := func(field, value string) {
		if value != "" {
			args = append(args, value)
			parts = append(parts, fmt.Sprintf("%s=$%d", field, len(args)))
		}
	}
	if refund {
		add("payment_intent_id", p.PaymentIntentID)
	} else {
		add("customer_id", p.CustomerID)
	}
	add("status", p.Status)
	return strings.Join(parts, " AND "), args
}
