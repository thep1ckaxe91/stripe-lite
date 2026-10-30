package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

type Intent struct {
	ID                  string    `json:"id"`
	Object              string    `json:"object"`
	CustomerID          string    `json:"customer_id"`
	PaymentMethodID     *string   `json:"payment_method_id"`
	Amount              int64     `json:"amount"`
	Currency            string    `json:"currency"`
	Status              string    `json:"status"`
	CaptureMethod       string    `json:"capture_method"`
	AmountCapturable    int64     `json:"amount_capturable"`
	AmountReceived      int64     `json:"amount_received"`
	AmountRefunded      int64     `json:"amount_refunded"`
	AmountRefundPending int64     `json:"amount_refund_pending"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}
type Refund struct {
	ID              string            `json:"id"`
	Object          string            `json:"object"`
	PaymentIntentID string            `json:"payment_intent_id"`
	Amount          int64             `json:"amount"`
	Currency        string            `json:"currency"`
	Status          string            `json:"status"`
	Reason          *string           `json:"reason,omitempty"`
	Metadata        map[string]string `json:"metadata"`
	CreatedAt       time.Time         `json:"created_at"`
}
type CreateIntent struct {
	Amount          int64   `json:"amount"`
	Currency        string  `json:"currency"`
	CustomerID      string  `json:"customer_id"`
	CaptureMethod   string  `json:"capture_method"`
	PaymentMethodID *string `json:"payment_method_id"`
}
type CreateRefund struct {
	PaymentIntentID string            `json:"payment_intent_id"`
	Amount          *int64            `json:"amount"`
	Reason          *string           `json:"reason"`
	Metadata        map[string]string `json:"metadata"`
}
type PageQuery struct {
	StartingAfter, EndingBefore, CustomerID, PaymentIntentID, Status string
	Limit                                                            int
}
type Page[T any] struct {
	Object  string `json:"object"`
	Data    []T    `json:"data"`
	HasMore bool   `json:"has_more"`
	URL     string `json:"url"`
}
type Error struct {
	Status       int
	Code, Detail string
}

func (e *Error) Error() string                   { return e.Detail }
func Fail(status int, code, detail string) error { return &Error{status, code, detail} }

var (
	ErrNotFound    = Fail(404, "not-found", "The resource was not found.")
	ErrConflict    = Fail(409, "invalid-state", "The current state does not permit this operation.")
	ErrUnavailable = Fail(503, "processor-unavailable", "The required service is unavailable. No financial change was made.")
	ErrDeclined    = errors.New("payment authorization declined")
	idPattern      = regexp.MustCompile(`^(pi|re|cus|pm)_[A-Za-z0-9]+$`)
)

func validID(id, prefix string) bool {
	return len(id) <= 255 && len(id) > len(prefix) && id[:len(prefix)] == prefix && idPattern.MatchString(id)
}
func bad(detail string) error { return Fail(400, "invalid-request", detail) }
func NormalizeError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{500, "internal-server-error", "The API could not complete the request. Retrieve the resource before retrying."}
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}
type Directory interface {
	Customer(context.Context, Querier, string, string) (bool, error)
	Method(context.Context, Querier, string, string, string) (bool, error)
}

// SQLDirectory reads Identity-owned records without changing their schema.
type SQLDirectory struct{}

func (SQLDirectory) Customer(ctx context.Context, q Querier, merchant, customer string) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM public.customers WHERE id::text=$1 AND merchant_id::text=$2)", customer, merchant).Scan(&found)
	if err != nil {
		return false, ErrUnavailable
	}
	return found, nil
}
func (SQLDirectory) Method(ctx context.Context, q Querier, merchant, customer, method string) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM public.payment_methods WHERE id::text=$1 AND merchant_id::text=$2 AND customer_id::text=$3)", method, merchant, customer).Scan(&found)
	if err != nil {
		return false, ErrUnavailable
	}
	return found, nil
}

type DemoDirectory struct{ Merchant, CustomerID string }

func (d DemoDirectory) Customer(_ context.Context, _ Querier, m, c string) (bool, error) {
	return m == d.Merchant && c == d.CustomerID, nil
}
func (d DemoDirectory) Method(_ context.Context, _ Querier, m, c, p string) (bool, error) {
	ok, _ := d.Customer(nil, nil, m, c)
	return ok && (p == "pm_demo" || p == "pm_declined" || p == "pm_unavailable" || p == "pm_refundfailed"), nil
}

type Processor interface {
	Authorize(context.Context, string, string, int64, string) error
	Capture(context.Context, string, string, int64, string) error
	Cancel(context.Context, string, string, int64, string) error
	Refund(context.Context, string, string, int64, string) error
}

// SimulatedProcessor is an explicit development adapter. It never moves real funds.
type SimulatedProcessor struct{}

func (SimulatedProcessor) Authorize(_ context.Context, _, method string, _ int64, _ string) error {
	switch method {
	case "pm_declined":
		return ErrDeclined
	case "pm_unavailable":
		return ErrUnavailable
	}
	return nil
}
func (SimulatedProcessor) Capture(context.Context, string, string, int64, string) error { return nil }
func (SimulatedProcessor) Cancel(context.Context, string, string, int64, string) error  { return nil }
func (SimulatedProcessor) Refund(_ context.Context, _, method string, _ int64, _ string) error {
	if method == "pm_refundfailed" {
		return ErrDeclined
	}
	return nil
}
func requireMethod(ctx context.Context, d Directory, q Querier, m, c, p string) error {
	if !validID(p, "pm_") {
		return bad("The payment method ID is invalid.")
	}
	ok, err := d.Method(ctx, q, m, c, p)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
func checkPage(p PageQuery) error {
	if p.Limit < 0 || p.Limit > 100 || p.StartingAfter != "" && p.EndingBefore != "" {
		return bad("The pagination parameters are invalid.")
	}
	return nil
}
func internal(err error) error { return fmt.Errorf("payments storage: %w", err) }
