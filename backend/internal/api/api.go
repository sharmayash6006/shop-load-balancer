// Package api contains the HTTP handlers of the shop backend.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"shop/backend/internal/auth"
	"shop/backend/internal/database"
	"shop/backend/internal/middleware"
)

type Server struct {
	DB           *sql.DB
	Log          *slog.Logger
	BackendID    string
	JWTSecret    []byte
	BcryptCost   int
	CookieSecure bool
	SameSite     http.SameSite // Lax (default), Strict, or None (needs Secure; for a site and API on different domains)
}

func (s *Server) sameSite() http.SameSite {
	if s.SameSite == 0 {
		return http.SameSiteLaxMode
	}
	return s.SameSite
}

type ctxKey int

const userKey ctxKey = 1

type Principal struct {
	ID   int64
	Name string
	SID  string
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /ready", s.health)

	mux.HandleFunc("POST /api/auth/register", s.register)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.Handle("GET /api/auth/me", s.authed(s.me))
	mux.Handle("GET /api/profile", s.authed(s.me))
	mux.Handle("PUT /api/profile", s.authed(s.updateProfile))

	mux.HandleFunc("GET /api/products", s.listProducts)
	mux.HandleFunc("GET /api/products/{id}", s.getProduct)

	mux.Handle("GET /api/cart", s.authed(s.getCart))
	mux.Handle("POST /api/cart", s.authed(s.addToCart))
	mux.Handle("DELETE /api/cart", s.authed(s.clearCart))
	mux.Handle("PUT /api/cart/items/{id}", s.authed(s.setCartQuantity))
	mux.Handle("DELETE /api/cart/items/{id}", s.authed(s.removeCartItem))

	mux.Handle("POST /api/checkout", s.authed(s.checkout))
	mux.Handle("POST /api/orders", s.authed(s.createOrder))
	mux.Handle("GET /api/orders", s.authed(s.listOrders))
	mux.Handle("GET /api/orders/{id}", s.authed(s.getOrder))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, 404, "not found") })
	return mux
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := database.Ping(r.Context(), s.DB); err != nil {
		w.Header().Set("X-Meta-Error", "DATABASE_UNAVAILABLE")
		writeJSON(w, 503, map[string]any{"status": "unhealthy", "backend": s.BackendID, "error": "database unavailable"})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "backend": s.BackendID})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// dbFail maps a database error to 503 (DB down) or 500 and logs it.
func (s *Server) dbFail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("database error", "request_id", middleware.RequestID(r.Context()), "error", err.Error())
	if database.IsUnavailable(err) {
		w.Header().Set("X-Meta-Error", "DATABASE_UNAVAILABLE")
		writeErr(w, 503, "database unavailable")
		return
	}
	writeErr(w, 500, "internal error")
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, 413, "request body too large")
		} else if errors.Is(err, io.EOF) {
			writeErr(w, 400, "empty body")
		} else {
			writeErr(w, 400, "invalid JSON body")
		}
		return false
	}
	return true
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// meta sets a response header the load balancer reads (and strips) to enrich its logs.
func meta(w http.ResponseWriter, key, val string) {
	val = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, val)
	if len(val) > 200 {
		val = val[:200]
	}
	w.Header().Set("X-Meta-"+key, val)
}

func userLabel(id int64) string { return fmt.Sprintf("USR-%d", id) }

// ---------- authentication middleware ----------

func (s *Server) authed(next func(http.ResponseWriter, *http.Request, Principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if c, err := r.Cookie("token"); err == nil {
			tok = c.Value
		} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		}
		claims, err := auth.Verify(tok, s.JWTSecret)
		if tok == "" || err != nil {
			writeErr(w, 401, "authentication required")
			return
		}
		meta(w, "User-ID", strconv.FormatInt(claims.Sub, 10))
		meta(w, "User-Name", claims.Name)
		next(w, r, Principal{ID: claims.Sub, Name: claims.Name, SID: claims.SID})
	})
}

var _ = context.Background
