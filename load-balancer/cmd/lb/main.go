package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"lb/internal/admin"
	"lb/internal/algorithms"
	"lb/internal/config"
	"lb/internal/health"
	"lb/internal/metrics"
	"lb/internal/middleware"
	"lb/internal/proxy"
	"lb/internal/routing"
	"lb/internal/store"
)

func main() {
	cfg := config.Load()
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	if len(cfg.JWTSecret) < 16 || cfg.AdminPassword == "" {
		log.Error("JWT_SECRET (>=16 chars) and ADMIN_PASSWORD must be set")
		os.Exit(1)
	}
	if !algorithms.Valid(cfg.Get().Algorithm) {
		cfg.Update(func(s *config.Settings) { s.Algorithm = algorithms.RoundRobin })
	}
	pool, err := routing.NewPool(cfg.BackendURLs, cfg.Weights)
	if err != nil || len(pool.All()) == 0 || len(pool.All()) > metrics.MaxBackends {
		log.Error("invalid BACKEND_URLS", "error", err)
		os.Exit(1)
	}
	m := metrics.New(pool)

	// The load balancer keeps routing even if the database is down: logging is best effort and non-blocking.
	var st *store.Store
	if s, err := store.Open(cfg.DatabaseURL, log, m); err != nil {
		log.Error("database config error, request logging disabled", "error", err)
	} else {
		st = s
		st.Start()
		m.Sink = st.LogRequest
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := st.WaitReady(ctx); err != nil {
				log.Error("database not reachable yet", "error", err)
				return
			}
			for _, b := range pool.All() {
				st.UpsertBackend(b)
			}
			log.Info("database connected")
		}()
	}

	mon := health.New(pool, cfg, log, st)
	stop := make(chan struct{})
	mon.CheckAll() // first round before accepting traffic so we do not 503 during startup
	go mon.Run(stop)
	go m.RunSampler(stop)

	px := proxy.New(pool, cfg, m, mon, log)
	adm := admin.New(cfg, pool, m, mon, st, log)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "uptime_seconds": int(time.Since(m.Started).Seconds())})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := pool.HealthyCount()
		if n == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": n > 0, "healthy_backends": n, "total_backends": len(pool.All())})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.WritePrometheus(w)
	})
	mux.Handle("/_lb/", adm.Handler())
	mux.Handle("/", px)

	handler := middleware.CORS(cfg.CORSOrigins, recoverer(log, middleware.SecureHeaders(mux)))
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("shutdown signal received: draining in-flight requests")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("forced shutdown", "error", err)
		}
		close(stop)
		if st != nil {
			st.Close() // flush buffered request logs and health checks
		}
		close(done)
	}()

	log.Info("load balancer listening", "addr", cfg.ListenAddr, "backends", strings.Join(cfg.BackendURLs, ","), "algorithm", cfg.Get().Algorithm)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	<-done
	log.Info("load balancer stopped cleanly")
}

func recoverer(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in handler", "panic", rec)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
