package payments

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type Principal struct {
	MerchantID string
	Scopes     map[string]bool
}
type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.MerchantID != ""
}

type Authenticator interface {
	Authenticate(*http.Request) (Principal, error)
}
type Credentials struct {
	keys             map[[32]byte]Principal
	secret           []byte
	issuer, audience string
}
type KeyConfig struct {
	Key        string   `json:"key"`
	MerchantID string   `json:"merchant_id"`
	Scopes     []string `json:"scopes"`
}

var ErrUnauthorized = Fail(401, "unauthorized", "Provide a valid bearer token or X-API-Key.")

func NewCredentials(config, secret, issuer, audience string) (*Credentials, error) {
	a := &Credentials{keys: map[[32]byte]Principal{}, secret: []byte(secret), issuer: issuer, audience: audience}
	if secret != "" && (len(secret) < 32 || issuer == "" || audience == "") {
		return nil, errors.New("JWT requires a secret of at least 32 bytes, an issuer, and an audience")
	}
	if config != "" {
		var values []KeyConfig
		if err := json.Unmarshal([]byte(config), &values); err != nil {
			return nil, errors.New("PAYMENTS_API_KEYS must be a JSON array of key configurations")
		}
		for _, v := range values {
			if len(v.Key) < 32 || strings.TrimSpace(v.MerchantID) == "" || len(v.MerchantID) > 255 {
				return nil, errors.New("API keys require 32 characters and a valid merchant ID")
			}
			p := Principal{v.MerchantID, map[string]bool{}}
			for _, s := range v.Scopes {
				switch s {
				case "payment_intents:read", "payment_intents:write", "refunds:read", "refunds:write":
					p.Scopes[s] = true
				default:
					return nil, errors.New("API key has an unsupported Payments scope")
				}
			}
			hash := sha256.Sum256([]byte(v.Key))
			if _, exists := a.keys[hash]; exists {
				return nil, errors.New("API key is configured more than once")
			}
			a.keys[hash] = p
		}
	}
	return a, nil
}

type tokenClaims struct {
	MerchantID string `json:"merchant_id"`
	Scope      string `json:"scope"`
	jwt.RegisteredClaims
}

func (a *Credentials) Authenticate(r *http.Request) (Principal, error) {
	if a == nil {
		return Principal{}, ErrUnauthorized
	}
	api, auth := r.Header.Values("X-API-Key"), r.Header.Values("Authorization")
	if len(api) > 1 || len(auth) > 1 || len(api) > 0 && len(auth) > 0 {
		return Principal{}, ErrUnauthorized
	}
	if len(api) == 1 {
		p, ok := a.keys[sha256.Sum256([]byte(api[0]))]
		if ok {
			return p, nil
		}
		return Principal{}, ErrUnauthorized
	}
	if len(auth) == 1 && len(a.secret) > 0 {
		parts := strings.Fields(auth[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			return Principal{}, ErrUnauthorized
		}
		claims := &tokenClaims{}
		token, err := jwt.ParseWithClaims(parts[1], claims, func(*jwt.Token) (any, error) { return a.secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(a.issuer), jwt.WithAudience(a.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
		if err == nil && token.Valid && strings.TrimSpace(claims.MerchantID) != "" && len(claims.MerchantID) <= 255 {
			p := Principal{claims.MerchantID, map[string]bool{}}
			for _, s := range strings.Fields(claims.Scope) {
				p.Scopes[s] = true
			}
			return p, nil
		}
	}
	return Principal{}, ErrUnauthorized
}
