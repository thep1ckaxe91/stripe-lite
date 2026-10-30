package payments

import (
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCredentials(t *testing.T) {
	key := strings.Repeat("k", 32)
	secret := strings.Repeat("s", 32)
	data, _ := json.Marshal([]KeyConfig{{key, "merchant_test", []string{"payment_intents:read"}}})
	a, err := NewCredentials(string(data), secret, "issuer", "audience")
	if err != nil {
		t.Fatal(err)
	}
	good := tokenClaims{MerchantID: "merchant_test", Scope: "refunds:write", RegisteredClaims: jwt.RegisteredClaims{Issuer: "issuer", Audience: []string{"audience"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	cases := []struct {
		name, api, auth string
		duplicate       bool
		want            bool
	}{
		{name: "missing"}, {name: "wrong key", api: "invalid"}, {name: "valid key", api: key, want: true}, {name: "dual credentials", api: key, auth: "Bearer token"}, {name: "duplicate key", api: key, duplicate: true},
	}
	sign := func(c tokenClaims, method jwt.SigningMethod, secret string) string {
		v, e := jwt.NewWithClaims(method, c).SignedString([]byte(secret))
		if e != nil {
			t.Fatal(e)
		}
		return "Bearer " + v
	}
	cases = append(cases, struct {
		name, api, auth string
		duplicate       bool
		want            bool
	}{name: "valid bearer", auth: sign(good, jwt.SigningMethodHS256, secret), want: true})
	for _, change := range []string{"signature", "algorithm", "expiration", "issuer", "audience", "merchant", "no expiration"} {
		c := good
		method := jwt.SigningMethodHS256
		signSecret := secret
		switch change {
		case "signature":
			signSecret = strings.Repeat("x", 32)
		case "algorithm":
			method = jwt.SigningMethodHS384
		case "expiration":
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		case "issuer":
			c.Issuer = "wrong"
		case "audience":
			c.Audience = []string{"wrong"}
		case "merchant":
			c.MerchantID = ""
		case "no expiration":
			c.ExpiresAt = nil
		}
		cases = append(cases, struct {
			name, api, auth string
			duplicate       bool
			want            bool
		}{name: change, auth: sign(c, method, signSecret)})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/refunds", nil)
			if c.api != "" {
				r.Header.Set("X-API-Key", c.api)
			}
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			if c.duplicate {
				r.Header.Add("X-API-Key", key)
			}
			p, e := a.Authenticate(r)
			if (e == nil) != c.want {
				t.Fatalf("authenticated=%v want %v", e == nil, c.want)
			}
			if c.want && p.MerchantID != "merchant_test" {
				t.Fatal("wrong merchant")
			}
		})
	}
	if _, e := NewCredentials(string(data), "short", "issuer", "audience"); e == nil {
		t.Fatal("accepted weak secret")
	}
}
