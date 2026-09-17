package api

import (
	"net/http"
	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"
)

func (s *Server) Routes() http.Handler{
	r:= chi.NewRouter()

	//rate limiter: 10 requests per second with a burst of 20
	limiter:= NewIPRateLimiter(rate.Limit(10), 20)

	//global middleware pipeline
	r.Use(Recoverer)
	r.Use(RequestLogger)
	r.Use(limiter.LimitMiddleware)

	//health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request){
		JSONResponse(w, http.StatusOK, map[string]string{"status": "healthy"})
	})

	//api v1 subrouter
	r.Route("/api/v1", func(r chi.Router){
		r.Post("/transfers", s.HandleTransfer)
		r.Get("/account/balance", s.HandleGetBalance)
	})

	return r
}