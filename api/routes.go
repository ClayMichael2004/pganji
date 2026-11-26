package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
	"pganji/metrics"
)

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	// Rate limiter: 10 requests per second with a burst of 20
	limiter := NewIPRateLimiter(rate.Limit(10), 20)

	// Global middleware pipeline
	r.Use(metrics.PrometheusMiddleware)
	r.Use(Recoverer)
	r.Use(RequestLogger)
	r.Use(limiter.LimitMiddleware)

	// Prometheus scraper endpoint
	r.Handle("/metrics", promhttp.Handler())

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		JSONResponse(w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	// API v1 subrouter
	r.Route("/api/v1", func(r chi.Router) {
		// Public Auth Endpoints
		r.Post("/auth/register", s.HandleRegister)
		r.Post("/auth/login", s.HandleLogin)

		// Public API Documentation & OpenAPI 3.1 Specification
		r.Get("/openapi.yaml", s.HandleOpenAPISpec)
		r.Head("/openapi.yaml", s.HandleOpenAPISpec)
		r.Get("/docs", s.HandleSwaggerUI)
		r.Head("/docs", s.HandleSwaggerUI)

		// External Provider Settlement Webhooks (HMAC-SHA256 authenticated)
		r.Post("/webhooks/mpesa/c2b", s.HandleMpesaDepositWebhook)
		r.Post("/webhooks/mpesa/b2c/result", s.HandleMpesaB2CResultWebhook)
		r.Post("/webhooks/mpesa/b2c/timeout", s.HandleMpesaB2CTimeoutWebhook)

		// Protected Financial Endpoints (JWT Authentication required)
		r.Group(func(protected chi.Router) {
			protected.Use(AuthMiddleware)

			// Multi-currency wallets, balances & statement history
			protected.Post("/accounts", s.HandleCreateAccount)
			protected.Get("/accounts", s.HandleListAccounts)
			protected.Get("/accounts/statement", s.HandleGetAccountStatement)
			protected.Get("/accounts/balance", s.HandleGetBalance)
			protected.Get("/account/balance", s.HandleGetBalance)

			// Treasury & Cryptographic Audit Endpoints (AuthMiddleware -> RequireAdmin)
			protected.With(s.RequireAdmin).Get("/audit/verify-chain", s.HandleVerifyLedgerChain)
			protected.With(s.RequireAdmin).Get("/audit/reconcile", s.HandleReconciliationReport)

			// Idempotent Money Movement
			protected.Group(func(idemp chi.Router) {
				idemp.Use(IdempotencyMiddleware(s.Pool))
				idemp.Post("/transfers", s.HandleTransfer)
				idemp.Post("/transfers/reverse", s.HandleReverseTransaction)
				idemp.Post("/clearing/withdraw", s.HandleWithdraw)
				idemp.Post("/fx/convert", s.HandleFXConvert)
			})
		})
	})

	return r
}
