package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	// HeaderRequestID is the correlation ID header propagated across gateway boundaries.
	HeaderRequestID     = "X-Request-Id"
	requestIDContextKey = "gateway.request_id"
	problemErrorBaseURI = "https://api.stripe-lite.internal/errors/"
	problemContentType  = "application/problem+json"
)

// ProblemDocument represents an RFC 7807 HTTP problem response payload.
type ProblemDocument struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// RequestID propagates an incoming valid UUID X-Request-Id header or generates a fresh one.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		EnsureRequestID(c)
		c.Next()
	}
}

// EnsureRequestID guarantees X-Request-Id is set on the response writer and Gin context.
func EnsureRequestID(c *gin.Context) string {
	if existing, ok := c.Get(requestIDContextKey); ok {
		if id, valid := existing.(string); valid && id != "" {
			if c.Writer.Header().Get(HeaderRequestID) == "" {
				c.Header(HeaderRequestID, id)
			}
			return id
		}
	}

	if existingHeader := c.Writer.Header().Get(HeaderRequestID); existingHeader != "" {
		if parsed, err := uuid.Parse(existingHeader); err == nil {
			id := parsed.String()
			c.Set(requestIDContextKey, id)
			return id
		}
	}

	requestID := uuid.NewString()
	if parsed, err := uuid.Parse(c.GetHeader(HeaderRequestID)); err == nil {
		requestID = parsed.String()
	}
	c.Header(HeaderRequestID, requestID)
	c.Set(requestIDContextKey, requestID)
	return requestID
}

// WriteProblem aborts the request chain with an RFC 7807 application/problem+json payload.
func WriteProblem(c *gin.Context, status int, errCode, title, detail string) {
	requestID := EnsureRequestID(c)
	if title == "" {
		title = http.StatusText(status)
	}
	c.Header("Content-Type", problemContentType)
	c.AbortWithStatusJSON(status, ProblemDocument{
		Type:     problemErrorBaseURI + errCode,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: "/requests/" + requestID,
	})
}
