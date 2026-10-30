package payments

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Runtime struct {
	Service     *Service
	Credentials *Credentials
	RateLimit   int
}

func LoadRuntime(ctx context.Context) (*Runtime, error) {
	credentials, err := NewCredentials(os.Getenv("PAYMENTS_API_KEYS"), os.Getenv("PAYMENTS_JWT_SECRET"), os.Getenv("PAYMENTS_JWT_ISSUER"), os.Getenv("PAYMENTS_JWT_AUDIENCE"))
	if err != nil {
		return nil, err
	}
	r := &Runtime{Credentials: credentials, RateLimit: 120}
	if v := os.Getenv("PAYMENTS_RATE_LIMIT"); v != "" {
		r.RateLimit, err = strconv.Atoi(v)
		if err != nil || r.RateLimit < 1 {
			return nil, errors.New("PAYMENTS_RATE_LIMIT must be positive")
		}
	}
	var directory Directory = SQLDirectory{}
	var processor Processor
	if mode := os.Getenv("PAYMENTS_PROCESSOR"); mode != "" {
		if mode != "simulated" {
			return nil, errors.New("no adapter is registered for PAYMENTS_PROCESSOR")
		}
		if os.Getenv("APP_ENV") == "production" {
			return nil, errors.New("the simulated processor is disabled in production")
		}
		merchant, customer := os.Getenv("PAYMENTS_DEMO_MERCHANT_ID"), os.Getenv("PAYMENTS_DEMO_CUSTOMER_ID")
		if merchant == "" {
			merchant = "merchant_demo"
		}
		if customer == "" {
			customer = "cus_demo"
		}
		directory = DemoDirectory{merchant, customer}
		processor = SimulatedProcessor{}
	}
	if uri := os.Getenv("DATABASE_URL"); uri != "" {
		cfg, e := pgxpool.ParseConfig(uri)
		if e != nil {
			return nil, errors.New("DATABASE_URL is invalid")
		}
		cfg.MaxConns = 20
		db, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			return nil, errors.New("database initialization failed")
		}
		if e = db.Ping(ctx); e != nil {
			db.Close()
			return nil, errors.New("the payment database is unavailable")
		}
		r.Service = New(db, directory, processor)
	}
	return r, nil
}
func (r *Runtime) Close() {
	if r != nil && r.Service != nil && r.Service.DB != nil {
		r.Service.DB.Close()
	}
}
func (r *Runtime) RunRefunds(ctx context.Context) {
	if r.Service == nil || r.Service.Processor == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			batch, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := r.Service.SettlePending(batch); err != nil && batch.Err() == nil {
				log.Printf("Payments refund worker: %s", NormalizeError(err).Code)
			}
			cancel()
		}
	}
}
