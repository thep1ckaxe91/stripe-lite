package v1

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

// Server implements StrictServerInterface for Stripe-lite v1 API gateway.
type Server struct {
	version   string
	payments  *payments.Service
	auth      payments.Authenticator
	rateLimit int
}

// NewServer constructs a new baseline Server instance.
func NewServer(version string, options ...func(*Server)) *Server {
	s := &Server{version: version, rateLimit: 120}
	for _, option := range options {
		option(s)
	}
	return s
}
func WithPayments(service *payments.Service, auth payments.Authenticator, rateLimit int) func(*Server) {
	return func(s *Server) {
		s.payments, s.auth = service, auth
		if rateLimit > 0 {
			s.rateLimit = rateLimit
		}
	}
}

var _ StrictServerInterface = (*Server)(nil)

// newNotImplementedProblem builds a standardized RFC 7807 problem document for feature stubs.
func newNotImplementedProblem(instance, detail string) ProblemDocument {
	return ProblemDocument{
		Type:     "https://api.stripe-lite.internal/errors/not-implemented",
		Title:    "Feature Not Implemented",
		Status:   http.StatusNotImplemented,
		Detail:   &detail,
		Instance: &instance,
	}
}

// NotFoundHandler returns the common problem document for an unknown route.
func NotFoundHandler(c *gin.Context) {
	detail := "The requested route was not found."
	instance := c.Request.URL.Path
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(http.StatusNotFound, ProblemDocument{
		Type: "https://api.stripe-lite.internal/errors/not-found", Title: http.StatusText(http.StatusNotFound),
		Status: http.StatusNotFound, Detail: &detail, Instance: &instance,
	})
}
