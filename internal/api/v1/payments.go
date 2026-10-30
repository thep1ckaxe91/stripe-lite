package v1

import (
	"context"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

func (s *Server) merchant(ctx context.Context) (string, error) {
	p, ok := payments.PrincipalFrom(ctx)
	if !ok {
		return "", payments.ErrUnauthorized
	}
	if s.payments == nil {
		return "", payments.ErrUnavailable
	}
	return p.MerchantID, nil
}
func intentModel(p payments.Intent) PaymentIntentResponse {
	return PaymentIntentResponse{Id: p.ID, Object: PaymentIntentResponseObject(p.Object), CustomerId: p.CustomerID, PaymentMethodId: p.PaymentMethodID, Amount: p.Amount, Currency: p.Currency, Status: PaymentIntentStatus(p.Status), CaptureMethod: CaptureMethod(p.CaptureMethod), AmountCapturable: p.AmountCapturable, AmountReceived: p.AmountReceived, AmountRefunded: p.AmountRefunded, AmountRefundPending: p.AmountRefundPending, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}
func refundModel(p payments.Refund) RefundResponse {
	var reason *RefundReason
	if p.Reason != nil {
		r := RefundReason(*p.Reason)
		reason = &r
	}
	return RefundResponse{Id: p.ID, Object: RefundResponseObject(p.Object), PaymentIntentId: p.PaymentIntentID, Amount: p.Amount, Currency: p.Currency, Status: RefundStatus(p.Status), Reason: reason, Metadata: p.Metadata, CreatedAt: p.CreatedAt}
}
func value[T ~string | ~int](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}

func (s *Server) CreatePaymentIntent(ctx context.Context, r CreatePaymentIntentRequestObject) (CreatePaymentIntentResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	if r.Body == nil {
		return problem(ctx, payments.Fail(400, "invalid-request", "A request body is required.")), nil
	}
	b := r.Body
	p, err := s.payments.CreateIntent(ctx, m, r.Params.IdempotencyKey, payments.CreateIntent{Amount: b.Amount, Currency: b.Currency, CustomerID: b.CustomerId, CaptureMethod: string(b.CaptureMethod), PaymentMethodID: b.PaymentMethodId})
	if err != nil {
		return problem(ctx, err), nil
	}
	return CreatePaymentIntent201JSONResponse(intentModel(p)), nil
}
func (s *Server) GetPaymentIntent(ctx context.Context, r GetPaymentIntentRequestObject) (GetPaymentIntentResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	p, err := s.payments.GetIntent(ctx, m, r.Id)
	if err != nil {
		return problem(ctx, err), nil
	}
	return GetPaymentIntent200JSONResponse(intentModel(p)), nil
}
func (s *Server) ConfirmPaymentIntent(ctx context.Context, r ConfirmPaymentIntentRequestObject) (ConfirmPaymentIntentResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	if r.Body == nil {
		return problem(ctx, payments.Fail(400, "invalid-request", "A request body is required.")), nil
	}
	p, err := s.payments.Confirm(ctx, m, r.Id, r.Body.PaymentMethodId)
	if err != nil {
		return problem(ctx, err), nil
	}
	return ConfirmPaymentIntent200JSONResponse(intentModel(p)), nil
}
func (s *Server) CapturePaymentIntent(ctx context.Context, r CapturePaymentIntentRequestObject) (CapturePaymentIntentResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	p, err := s.payments.Capture(ctx, m, r.Id)
	if err != nil {
		return problem(ctx, err), nil
	}
	return CapturePaymentIntent200JSONResponse(intentModel(p)), nil
}
func (s *Server) CancelPaymentIntent(ctx context.Context, r CancelPaymentIntentRequestObject) (CancelPaymentIntentResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	p, err := s.payments.Cancel(ctx, m, r.Id)
	if err != nil {
		return problem(ctx, err), nil
	}
	return CancelPaymentIntent200JSONResponse(intentModel(p)), nil
}
func (s *Server) ListPaymentIntents(ctx context.Context, r ListPaymentIntentsRequestObject) (ListPaymentIntentsResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	q := r.Params
	p, err := s.payments.ListIntents(ctx, m, payments.PageQuery{StartingAfter: value(q.StartingAfter), EndingBefore: value(q.EndingBefore), Limit: value(q.Limit), CustomerID: value(q.CustomerId), Status: string(value(q.Status))})
	if err != nil {
		return problem(ctx, err), nil
	}
	data := make([]PaymentIntentResponse, len(p.Data))
	for i, v := range p.Data {
		data[i] = intentModel(v)
	}
	return ListPaymentIntents200JSONResponse{Data: data, HasMore: p.HasMore, Object: PaymentIntentListObject(p.Object), Url: PaymentIntentListUrl(p.URL)}, nil
}
func (s *Server) CreateRefund(ctx context.Context, r CreateRefundRequestObject) (CreateRefundResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	if r.Body == nil {
		return problem(ctx, payments.Fail(400, "invalid-request", "A request body is required.")), nil
	}
	b := r.Body
	var reason *string
	if b.Reason != nil {
		v := string(*b.Reason)
		reason = &v
	}
	var metadata map[string]string
	if b.Metadata != nil {
		metadata = *b.Metadata
	}
	p, err := s.payments.CreateRefund(ctx, m, r.Params.IdempotencyKey, payments.CreateRefund{PaymentIntentID: b.PaymentIntentId, Amount: b.Amount, Reason: reason, Metadata: metadata})
	if err != nil {
		return problem(ctx, err), nil
	}
	return CreateRefund201JSONResponse(refundModel(p)), nil
}
func (s *Server) GetRefund(ctx context.Context, r GetRefundRequestObject) (GetRefundResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	p, err := s.payments.GetRefund(ctx, m, r.Id)
	if err != nil {
		return problem(ctx, err), nil
	}
	return GetRefund200JSONResponse(refundModel(p)), nil
}
func (s *Server) ListRefunds(ctx context.Context, r ListRefundsRequestObject) (ListRefundsResponseObject, error) {
	m, err := s.merchant(ctx)
	if err != nil {
		return problem(ctx, err), nil
	}
	q := r.Params
	p, err := s.payments.ListRefunds(ctx, m, payments.PageQuery{StartingAfter: value(q.StartingAfter), EndingBefore: value(q.EndingBefore), Limit: value(q.Limit), PaymentIntentID: value(q.PaymentIntentId), Status: string(value(q.Status))})
	if err != nil {
		return problem(ctx, err), nil
	}
	data := make([]RefundResponse, len(p.Data))
	for i, v := range p.Data {
		data[i] = refundModel(v)
	}
	return ListRefunds200JSONResponse{Data: data, HasMore: p.HasMore, Object: RefundListObject(p.Object), Url: RefundListUrl(p.URL)}, nil
}
