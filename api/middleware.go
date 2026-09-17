package api

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
	"golang.org/x/time/rate"
)

//this function logs incoming http method, url path and execution latency
func RequestLogger(next http.Handler) http.Handler{
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request){
	start:= time.Now()
	next.ServeHTTP(w, r)
	slog.Info("http request",
	"method", r.Method,
	"path", r.URL.Path,
	"duration", time.Since(start).String(),
	)
	})
}

func Recoverer(next http.Handler)http.Handler{
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request){
		defer func(){
			if rvr := recover(); rvr != nil{
				slog.Error("server panic recovered", "error", rvr)
				ErrorResponse(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

//represents a rate limited client bucket
type client struct{
	limiter *rate.Limiter
	lastSeen time.Time
}

//manages per-IP Tocken Bucket limiters
type IPRateLimiter struct{
	mu sync.Mutex
	clients map[string]*client
	rate rate.Limiter
	burst int
}

//function below creates a new rate limiter 
func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter{
	limiter := &IPRateLimiter{
		clients: make(map[string]*client),
		rate: r,
		burst; b,
	}

	//clean up inactive clients every 5 minutes in the background
	go limiter.cleanuploop()
	return limiter
}

func (i *IPRateLimiter) getClient(ip string) *rate.Limiter{
	i.mu.Lock()
	defer i.mu.Unlock()

	c, exists:= i.clients[ip]
	if !exists{
		limiter:= rate.NewLimiter(i.rate, i.burst)
		i.clients[ip]=&client{limiter: limiter, lastSeen: time.Now()}
		return limiter
	}
	c.lastSeen= time.Now()
	return c.limiter
}

func (i *IPRateLimiter) LimitMiddleware(next http.Handler) http.Handler{
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request){
		ip, _, err:= net.SplitHostPort(r.RemoteAddr)
		if err !=nil{
			ip = r.RemoteAddr
		}
		limiter:= i.getClient(ip)
		if !limiter.Allow(){
			slog.Warn("rate limit exceeded", "ip", ip)
			ErrorResponse(w, http.StatusTooManyRequests, "rate limit exceeding: try again later")
			return
		}
		next.ServeHTTP(w, r)
	})
}