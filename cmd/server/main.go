package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	v1 "github.com/thep1ckaxe91/stripe-lite/internal/api/v1"
	"github.com/thep1ckaxe91/stripe-lite/internal/config"
	"github.com/thep1ckaxe91/stripe-lite/internal/docs"
	"github.com/thep1ckaxe91/stripe-lite/internal/payments"
)

const version = "1.0.0"

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Configuration bootstrap failure: %v", err)
	}

	if cfg.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	startup, done := context.WithTimeout(context.Background(), 5*time.Second)
	runtime, err := payments.LoadRuntime(startup)
	done()
	if err != nil {
		log.Fatalf("Payments configuration: %v", err)
	}
	defer runtime.Close()
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); runtime.RunRefunds(workerCtx) }()

	router := gin.New()
	router.ContextWithFallback = true
	router.Use(gin.Recovery())
	router.Use(gin.Logger())

	// Mount documentation & OpenAPI schemas
	if err := docs.RegisterRoutes(router); err != nil {
		log.Fatalf("Failed to initialize documentation routes: %v", err)
	}

	// Root-level health probe alias for container orchestrators (k8s/nomad)
	router.GET("/healthz", func(c *gin.Context) {
		c.Redirect(http.StatusPermanentRedirect, cfg.BasePath+"/healthz")
	})

	// Instantiate strictly-typed domain server
	domainServer := v1.NewServer(version, v1.WithPayments(runtime.Service, runtime.Credentials, runtime.RateLimit))
	paymentMiddleware, err := domainServer.PaymentMiddleware(cfg.BasePath)
	if err != nil {
		log.Fatalf("Payments contract: %v", err)
	}
	router.Use(paymentMiddleware)
	strictHandler := v1.NewStrictHandler(domainServer, nil)

	// Register generated Gin routes with strict server contract
	v1.RegisterHandlersWithOptions(router, strictHandler, v1.GinServerOptions{
		BaseURL:      cfg.BasePath,
		ErrorHandler: v1.PaymentBindingError,
	})

	router.NoRoute(v1.NotFoundHandler)

	serverAddr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{
		Addr:              serverAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("[Server] Starting Stripe-lite API gateway on %s (base: %s, env: %s)",
			serverAddr, cfg.BasePath, cfg.Environment)
		log.Printf("[Server] Swagger UI available at http://%s/docs", serverAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server listen error: %v", err)
		}
	}()

	// Graceful termination handling
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("[Server] Graceful shutdown sequence initiated...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("[Server] Forced server termination: %v", err)
	}

	stopWorker()
	<-workerDone
	log.Println("[Server] Gateway shut down cleanly.")
}
