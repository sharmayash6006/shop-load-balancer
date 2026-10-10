package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"shop/backend/internal/auth"
	"shop/backend/internal/database"
	"shop/backend/internal/models"
)

const tokenTTL = 24 * time.Hour

// dummyHash is compared against when the email is unknown so response time does not reveal
// whether an account exists.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)

func (s *Server) issueSession(w http.ResponseWriter, u models.User) {
	sid := make([]byte, 8)
	_, _ = rand.Read(sid)
	tok := auth.Sign(auth.Claims{Sub: u.ID, Name: u.Name, SID: hex.EncodeToString(sid), Exp: time.Now().Add(tokenTTL).Unix()}, s.JWTSecret)
	http.SetCookie(w, &http.Cookie{Name: "token", Value: tok, Path: "/", HttpOnly: true, Secure: s.CookieSecure,
		SameSite: s.sameSite(), MaxAge: int(tokenTTL.Seconds())})
}

type credentials struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 100 {
		writeErr(w, 400, "name must be 1-100 characters")
		return
	}
	if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email || len(in.Email) > 254 {
		writeErr(w, 400, "invalid email address")
		return
	}
	if len(in.Password) < 8 || len(in.Password) > 72 {
		writeErr(w, 400, "password must be 8-72 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), s.BcryptCost)
	if err != nil {
		writeErr(w, 500, "internal error")
		return
	}
	u := models.User{Name: in.Name, Email: in.Email}
	err = s.DB.QueryRowContext(r.Context(),
		`INSERT INTO users (name, email, password_hash) VALUES ($1,$2,$3) RETURNING id, created_at`,
		u.Name, u.Email, string(hash)).Scan(&u.ID, &u.CreatedAt)
	if database.IsUniqueViolation(err) {
		writeErr(w, 409, "email already registered")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	meta(w, "User-ID", strconv.FormatInt(u.ID, 10))
	meta(w, "User-Name", u.Name)
	s.issueSession(w, u)
	writeJSON(w, 201, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decode(w, r, &in) {
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	var u models.User
	var hash string
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT id, name, email, password_hash, created_at FROM users WHERE email=$1`, in.Email).
		Scan(&u.ID, &u.Name, &u.Email, &hash, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
		writeErr(w, 401, "invalid email or password")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		writeErr(w, 401, "invalid email or password")
		return
	}
	meta(w, "User-ID", strconv.FormatInt(u.ID, 10))
	meta(w, "User-Name", u.Name)
	s.issueSession(w, u)
	writeJSON(w, 200, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "token", Value: "", Path: "/", HttpOnly: true, Secure: s.CookieSecure,
		SameSite: s.sameSite(), MaxAge: -1})
	writeJSON(w, 200, map[string]string{"status": "logged out"})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, p Principal) {
	var u models.User
	var orders int
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT u.id, u.name, u.email, u.created_at, (SELECT count(*) FROM orders o WHERE o.user_id=u.id)
		   FROM users u WHERE u.id=$1`, p.ID).Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &orders)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 401, "account no longer exists")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "created_at": u.CreatedAt, "order_count": orders})
}

func (s *Server) updateProfile(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 100 {
		writeErr(w, 400, "name must be 1-100 characters")
		return
	}
	var u models.User
	err := s.DB.QueryRowContext(r.Context(),
		`UPDATE users SET name=$1 WHERE id=$2 RETURNING id, name, email, created_at`, in.Name, p.ID).
		Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	s.issueSession(w, u) // refresh the name embedded in the token
	writeJSON(w, 200, u)
}
