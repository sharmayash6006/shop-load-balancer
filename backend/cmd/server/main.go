package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"shop/backend/internal/api"
	"shop/backend/internal/database"
	"shop/backend/internal/middleware"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(env("LOG_LEVEL", "info")))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	id := env("BACKEND_ID", "backend-1")
	port := env("PORT", "8001")
	cost, _ := strconv.Atoi(env("BCRYPT_COST", "10"))
	db, err := database.Open(env("DATABASE_URL", "postgres://shop:shop@localhost:5432/shop?sslmode=disable"))
	if err != nil {
		log.Error("cannot open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	secret := env("JWT_SECRET", "")
	if len(secret) < 16 {
		log.Error("JWT_SECRET must be set (>= 16 characters)")
		os.Exit(1)
	}
	srv := &api.Server{DB: db, Log: log, BackendID: id, JWTSecret: []byte(secret), BcryptCost: cost,
		CookieSecure: env("COOKIE_SECURE", "false") == "true", SameSite: sameSite(env("COOKIE_SAMESITE", "lax"))}
	if srv.SameSite == http.SameSiteNoneMode && !srv.CookieSecure {
		log.Warn("COOKIE_SAMESITE=none requires COOKIE_SECURE=true: browsers will reject the session cookie")
	}

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           middleware.Chain(srv.Routes(), id, 1<<20, log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	// Graceful shutdown: stop accepting, finish in-flight requests, close the DB pool.
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Info("shutdown signal received, draining", "backend", id)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Error("shutdown error", "error", err)
		}
		close(done)
	}()

	log.Info("backend listening", "backend", id, "port", port)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
	<-done
	log.Info("backend stopped cleanly", "backend", id)
}

func sameSite(v string) http.SameSite {
	switch v {
	case "none":
		return http.SameSiteNoneMode
	case "strict":
		return http.SameSiteStrictMode
	}
	return http.SameSiteLaxMode
}
