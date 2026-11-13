package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

// Subject identifies the caller for gateway rate limiting and idempotency namespacing.
type Subject struct {
	Key        string
	MerchantID string
	IsMerchant bool
}

// SubjectResolver extracts the caller identity from an incoming HTTP request.
type SubjectResolver func(c *gin.Context) Subject

// NewSubjectResolver builds a SubjectResolver backed by an optional payments.Authenticator.
// Authenticated principals are cached on the request context so downstream domain middleware
// does not need to re-verify credentials. Unauthenticated callers fall back to client IP.
func NewSubjectResolver(auth payments.Authenticator) SubjectResolver {
	return func(c *gin.Context) Subject {
		if p, ok := payments.PrincipalFrom(c.Request.Context()); ok {
			return Subject{
				Key:        "merchant:" + p.MerchantID,
				MerchantID: p.MerchantID,
				IsMerchant: true,
			}
		}

		if auth != nil {
			if p, err := auth.Authenticate(c.Request); err == nil && p.MerchantID != "" {
				c.Request = c.Request.WithContext(payments.WithPrincipal(c.Request.Context(), p))
				return Subject{
					Key:        "merchant:" + p.MerchantID,
					MerchantID: p.MerchantID,
					IsMerchant: true,
				}
			}
		}

		ip := strings.TrimSpace(c.ClientIP())
		if ip == "" {
			ip = "unknown"
		}
		return Subject{
			Key:        "ip:" + ip,
			IsMerchant: false,
		}
	}
}
