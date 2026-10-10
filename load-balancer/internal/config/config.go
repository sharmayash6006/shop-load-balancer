// Package config holds startup configuration (env vars) and the live, admin-editable settings.
package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Settings are the runtime-tunable values. They can be changed from the dashboard without a restart.
type Settings struct {
	Algorithm        string  `json:"algorithm"`
	ConnectTimeoutMs int     `json:"connect_timeout_ms"`
	RequestTimeoutMs int     `json:"request_timeout_ms"`
	BackendTimeoutMs int     `json:"backend_timeout_ms"`
	RetryCount       int     `json:"retry_count"`
	RateLimit        float64 `json:"rate_limit"` // requests/second per client IP, 0 = disabled
	HealthIntervalMs int     `json:"health_interval_ms"`
	HealthTimeoutMs  int     `json:"health_timeout_ms"`
	HealthFall       int     `json:"health_fall"` // consecutive failures before UNHEALTHY
	HealthRise       int     `json:"health_rise"` // consecutive successes before HEALTHY again
}

// Config is the full static configuration plus the live settings.
type Config struct {
	ListenAddr     string
	DatabaseURL    string
	BackendURLs    []string
	Weights        []int
	LogLevel       string
	AdminUser      string
	AdminPassword  string
	JWTSecret      string
	MaxBodyBytes   int64
	CORSOrigins    []string
	CookieSecure   bool
	CookieSameSite string

	mu sync.RWMutex
	s  Settings
}

func (c *Config) Get() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.s
}

func (c *Config) Update(f func(*Settings)) Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.s)
	return c.s
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envDurMs(k string, def time.Duration) int {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return int(d.Milliseconds())
		}
		if n, err := strconv.Atoi(v); err == nil { // plain number = seconds
			return n * 1000
		}
	}
	return int(def.Milliseconds())
}

func Load() *Config {
	c := &Config{
		ListenAddr:     env("LISTEN_ADDR", ":8080"),
		DatabaseURL:    env("DATABASE_URL", "postgres://shop:shop@localhost:5432/shop?sslmode=disable"),
		LogLevel:       env("LOG_LEVEL", "info"),
		AdminUser:      env("ADMIN_USER", "admin"),
		AdminPassword:  env("ADMIN_PASSWORD", ""),
		JWTSecret:      env("JWT_SECRET", ""),
		MaxBodyBytes:   int64(envInt("MAX_BODY_BYTES", 1<<20)),
		CookieSecure:   env("COOKIE_SECURE", "false") == "true",
		CookieSameSite: env("COOKIE_SAMESITE", "lax"),
	}
	for _, u := range strings.Split(env("BACKEND_URLS", "http://localhost:8001,http://localhost:8002,http://localhost:8003"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.BackendURLs = append(c.BackendURLs, u)
		}
	}
	for _, w := range strings.Split(env("BACKEND_WEIGHTS", ""), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(w)); err == nil && n >= 1 {
			c.Weights = append(c.Weights, n)
		}
	}
	for _, o := range strings.Split(env("CORS_ORIGINS", ""), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	rate, _ := strconv.ParseFloat(env("RATE_LIMIT", "0"), 64)
	c.s = Settings{
		Algorithm:        env("ALGORITHM", "round_robin"),
		ConnectTimeoutMs: envDurMs("CONNECT_TIMEOUT", 2*time.Second),
		RequestTimeoutMs: envDurMs("REQUEST_TIMEOUT", 30*time.Second),
		BackendTimeoutMs: envDurMs("BACKEND_TIMEOUT", 10*time.Second),
		RetryCount:       envInt("RETRY_COUNT", 2),
		RateLimit:        rate,
		HealthIntervalMs: envDurMs("HEALTH_CHECK_INTERVAL", 2*time.Second),
		HealthTimeoutMs:  envDurMs("HEALTH_TIMEOUT", 2*time.Second),
		HealthFall:       envInt("HEALTH_FALL", 2),
		HealthRise:       envInt("HEALTH_RISE", 2),
	}
	return c
}
