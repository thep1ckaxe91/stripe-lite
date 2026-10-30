package payments

import (
	"context"
	"testing"
)

func TestRuntimeConfiguration(t *testing.T) {
	for _, name := range []string{"DATABASE_URL", "PAYMENTS_API_KEYS", "PAYMENTS_JWT_SECRET", "PAYMENTS_JWT_ISSUER", "PAYMENTS_JWT_AUDIENCE", "PAYMENTS_PROCESSOR", "PAYMENTS_RATE_LIMIT"} {
		t.Setenv(name, "")
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("PAYMENTS_PROCESSOR", "simulated")
	if _, e := LoadRuntime(context.Background()); e == nil {
		t.Fatal("production accepted the simulated processor")
	}
	t.Setenv("APP_ENV", "development")
	t.Setenv("PAYMENTS_PROCESSOR", "unknown")
	if _, e := LoadRuntime(context.Background()); e == nil {
		t.Fatal("unknown processor was accepted")
	}
	t.Setenv("PAYMENTS_PROCESSOR", "")
	t.Setenv("PAYMENTS_RATE_LIMIT", "0")
	if _, e := LoadRuntime(context.Background()); e == nil {
		t.Fatal("invalid rate limit was accepted")
	}
	t.Setenv("PAYMENTS_RATE_LIMIT", "")
	r, e := LoadRuntime(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if r.Service != nil {
		t.Fatal("missing database created a payment service")
	}
	r.RunRefunds(context.Background())
}
