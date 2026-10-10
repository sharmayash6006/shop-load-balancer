package database

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/lib/pq"
)

func Open(url string) (*sql.DB, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

func Ping(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// IsUnavailable reports whether err means the database itself cannot be reached
// (as opposed to a query/constraint error).
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var pe *pq.Error
	if errors.As(err, &pe) {
		c := string(pe.Code)
		return strings.HasPrefix(c, "08") || strings.HasPrefix(c, "57P") || strings.HasPrefix(c, "53")
	}
	m := err.Error()
	return strings.Contains(m, "connection refused") || strings.Contains(m, "bad connection") ||
		strings.Contains(m, "no such host") || strings.Contains(m, "database is closed")
}

// IsUniqueViolation reports a unique-constraint violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

// IsForeignKeyViolation reports SQLSTATE 23503.
func IsForeignKeyViolation(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23503"
}
