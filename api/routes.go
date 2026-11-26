package api

import (
	"net/http"
	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"pganji/metrics"
)

func (s *Server) Routes() http.Handler{
	r:= chi.NewRouter()

	//rate limiter: 10 requests per second with a burst of 20
	limiter:= NewIPRateLimiter(rate.Limit(10), 20)

	//global middleware pipeline
	r.Use(metrics.PrometheusMiddleware)
	r.Use(Recoverer)
	r.Use(RequestLogger)
	r.Use(limiter.LimitMiddleware)

	//promotheus scraper endpoint
	r.Handle("/metrics", promhttp.Handler())

	//health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request){
		JSONResponse(w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	//api v1 subrouter
	r.Route("/api/v1", func(r chi.Router) {
		// Public Auth Endpoints
		r.Post("/auth/register", s.HandleRegister)
		r.Post("/auth/login", s.HandleLogin)
		r.Post("/webhooks/mpesa/c2b", s.HandleMpesaDepositWebhook)
		// Protected Financial Endpoints
		r.Group(func(protected chi.Router) {
			protected.Use(AuthMiddleware)

			// Statement / History Pagination
			protected.Get("/accounts/statement", s.HandleGetAccountStatement)
			protected.Get("/accounts/balance", s.HandleGetBalance)
			protected.Get("/account/balance", s.HandleGetBalance)
			protected.Get("/audit/verify-chain", s.HandleVerifyLedgerChain)

			// Idempotent Money Movement
			protected.Group(func(idemp chi.Router) {
				idemp.Use(IdempotencyMiddleware(s.Pool))
				idemp.Post("/transfers", s.HandleTransfer)
				idemp.Post("/transfers/reverse", s.HandleReverseTransaction)
				idemp.Post("/clearing/withdraw", s.HandleWithdraw)
			})
		})
	})

	return r
}