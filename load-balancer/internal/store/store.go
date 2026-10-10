// Package store persists request logs, backend state and health checks in PostgreSQL and serves the
// admin queries (search, user activity, orders, order trace).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"lb/internal/metrics"
	"lb/internal/routing"
)

type HealthRecord struct {
	BackendID  string
	Time       time.Time
	Healthy    bool
	StatusCode int
	LatencyMs  float64
	Error      string
	State      string
	Transition bool
}

type Store struct {
	DB     *sql.DB
	log    *slog.Logger
	m      *metrics.Metrics
	reqCh  chan metrics.Entry
	hcCh   chan HealthRecord
	wg     sync.WaitGroup
	stopCh chan struct{}
}

func Open(url string, log *slog.Logger, m *metrics.Metrics) (*Store, error) {
	db, err := sql.Open("postgres", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &Store{DB: db, log: log, m: m, reqCh: make(chan metrics.Entry, 50000), hcCh: make(chan HealthRecord, 1000), stopCh: make(chan struct{})}, nil
}

// WaitReady blocks until the database answers (the LB keeps serving traffic even if this never succeeds).
func (s *Store) WaitReady(ctx context.Context) error {
	for {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.DB.PingContext(c)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
	}
}

// LogRequest never blocks the request path: when the buffer is full the row is dropped and counted.
func (s *Store) LogRequest(e metrics.Entry) {
	select {
	case s.reqCh <- e:
	default:
		s.m.DroppedLogs.Add(1)
	}
}

func (s *Store) LogHealth(r HealthRecord) {
	select {
	case s.hcCh <- r:
	default:
	}
}

func (s *Store) Start() {
	s.wg.Add(1)
	go s.loop()
}

// Close stops the writer after flushing everything that is buffered.
func (s *Store) Close() {
	close(s.stopCh)
	s.wg.Wait()
	_ = s.DB.Close()
}

func (s *Store) loop() {
	defer s.wg.Done()
	var reqs []metrics.Entry
	var hcs []HealthRecord
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	flush := func() {
		if len(reqs) > 0 {
			if err := s.copyRequests(reqs); err != nil {
				s.log.Error("request log flush failed", "error", err, "rows", len(reqs))
				s.m.DroppedLogs.Add(int64(len(reqs)))
			}
			reqs = reqs[:0]
		}
		if len(hcs) > 0 {
			if err := s.copyHealth(hcs); err != nil {
				s.log.Error("health log flush failed", "error", err)
			}
			hcs = hcs[:0]
		}
	}
	for {
		select {
		case e := <-s.reqCh:
			reqs = append(reqs, e)
			if len(reqs) >= 1000 {
				flush()
			}
		case h := <-s.hcCh:
			hcs = append(hcs, h)
		case <-t.C:
			flush()
		case <-s.stopCh:
			for {
				select {
				case e := <-s.reqCh:
					reqs = append(reqs, e)
				case h := <-s.hcCh:
					hcs = append(hcs, h)
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *Store) copyRequests(rows []metrics.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	st, err := tx.PrepareContext(ctx, pq.CopyIn("request_logs", "ts", "request_id", "user_id", "user_name", "client_ip", "method", "path",
		"action", "detail", "order_id", "backend_id", "status", "latency_ms", "request_size", "response_size", "success",
		"failure_reason", "retries", "load_test"))
	if err != nil {
		return err
	}
	for _, e := range rows {
		var uid any
		if e.UserID > 0 {
			uid = e.UserID
		}
		if _, err := st.ExecContext(ctx, e.Time, e.RequestID, uid, e.UserName, e.IP, e.Method, e.Path, e.Action, e.Detail, e.OrderID,
			e.BackendID, e.Status, e.LatencyMs, e.ReqSize, e.RespSize, e.Success, e.FailureReason, e.Retries, e.LoadTest != ""); err != nil {
			return err
		}
	}
	if _, err := st.ExecContext(ctx); err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) copyHealth(rows []HealthRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	st, err := tx.PrepareContext(ctx, pq.CopyIn("health_checks", "backend_id", "checked_at", "healthy", "status_code", "latency_ms", "error", "state", "transition"))
	if err != nil {
		return err
	}
	for _, h := range rows {
		if _, err := st.ExecContext(ctx, h.BackendID, h.Time, h.Healthy, h.StatusCode, h.LatencyMs, h.Error, h.State, h.Transition); err != nil {
			return err
		}
	}
	if _, err := st.ExecContext(ctx); err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

// UpsertBackend keeps the backend_servers table in sync with the running pool.
func (s *Store) UpsertBackend(b *routing.Backend) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO backend_servers (id, name, host, port, weight, enabled) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (id) DO UPDATE SET name=$2, host=$3, port=$4, weight=$5, enabled=$6, updated_at=now()`,
		b.ID, b.Name, b.Host, b.Port, b.Weight(), b.Enabled())
	if err != nil {
		s.log.Warn("backend upsert failed", "backend", b.ID, "error", err)
	}
}

// ---------------- admin queries ----------------

type ReqFilter struct {
	Q          string
	Status     string // "200", "2xx", "4xx", "5xx", "error" (>=400)
	Backend    string
	FailedOnly bool
	UserID     int64
	LoadTest   string // "", "only", "exclude"
	From, To   time.Time
	Limit      int
}

const entryCols = `id, ts, request_id, COALESCE(user_id,0), user_name, client_ip, method, path, action, detail, order_id, backend_id,
 status, latency_ms, request_size, response_size, success, failure_reason, retries, load_test`

func scanEntries(rows *sql.Rows) ([]metrics.Entry, error) {
	defer rows.Close()
	out := []metrics.Entry{}
	for rows.Next() {
		var e metrics.Entry
		var lt bool
		if err := rows.Scan(&e.Seq, &e.Time, &e.RequestID, &e.UserID, &e.UserName, &e.IP, &e.Method, &e.Path, &e.Action, &e.Detail,
			&e.OrderID, &e.BackendID, &e.Status, &e.LatencyMs, &e.ReqSize, &e.RespSize, &e.Success, &e.FailureReason, &e.Retries, &lt); err != nil {
			return nil, err
		}
		if lt {
			e.LoadTest = "k6"
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) Requests(ctx context.Context, f ReqFilter) ([]metrics.Entry, error) {
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	where = append(where, "path NOT LIKE '/_lb/%'")
	if q := strings.TrimSpace(f.Q); q != "" {
		p := arg("%" + escapeLike(q) + "%")
		c := fmt.Sprintf(`(request_id ILIKE %[1]s OR user_name ILIKE %[1]s OR client_ip ILIKE %[1]s OR path ILIKE %[1]s OR action ILIKE %[1]s
			OR order_id ILIKE %[1]s OR backend_id ILIKE %[1]s OR detail ILIKE %[1]s OR ('USR-' || COALESCE(user_id::text,'')) ILIKE %[1]s`, p)
		if n, err := strconv.Atoi(q); err == nil {
			c += " OR status = " + arg(n)
		}
		where = append(where, c+")")
	}
	switch st := strings.ToLower(f.Status); {
	case st == "error":
		where = append(where, "status >= 400")
	case len(st) == 3 && st[1:] == "xx":
		d := int(st[0] - '0')
		where = append(where, "status >= "+arg(d*100)+" AND status < "+arg(d*100+100))
	case st != "":
		if n, err := strconv.Atoi(st); err == nil {
			where = append(where, "status = "+arg(n))
		}
	}
	if f.Backend != "" {
		where = append(where, "backend_id = "+arg(f.Backend))
	}
	if f.FailedOnly {
		where = append(where, "NOT success")
	}
	if f.UserID > 0 {
		where = append(where, "user_id = "+arg(f.UserID))
	}
	switch f.LoadTest {
	case "only":
		where = append(where, "load_test")
	case "exclude":
		where = append(where, "NOT load_test")
	}
	if !f.From.IsZero() {
		where = append(where, "ts >= "+arg(f.From))
	}
	if !f.To.IsZero() {
		where = append(where, "ts <= "+arg(f.To))
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+entryCols+" FROM request_logs WHERE "+strings.Join(where, " AND ")+" ORDER BY ts DESC, id DESC LIMIT "+strconv.Itoa(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

type UserRow struct {
	ID           int64      `json:"id"`
	Label        string     `json:"label"`
	Name         string     `json:"name"`
	Email        string     `json:"email"`
	CreatedAt    time.Time  `json:"created_at"`
	Orders       int        `json:"orders"`
	Requests     int        `json:"requests"`
	LastActivity *time.Time `json:"last_activity"`
}

func (s *Store) Users(ctx context.Context, q string) ([]UserRow, error) {
	args := []any{}
	where := "TRUE"
	if q = strings.TrimSpace(q); q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		where = `(u.name ILIKE $1 OR u.email ILIKE $1 OR ('USR-' || u.id) ILIKE $1)`
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id, u.name, u.email, u.created_at,
		 (SELECT count(*) FROM orders o WHERE o.user_id=u.id),
		 (SELECT count(*) FROM request_logs r WHERE r.user_id=u.id),
		 (SELECT max(ts) FROM request_logs r WHERE r.user_id=u.id)
		 FROM users u WHERE `+where+` ORDER BY u.created_at DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRow{}
	for rows.Next() {
		var u UserRow
		var last sql.NullTime
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.CreatedAt, &u.Orders, &u.Requests, &last); err != nil {
			return nil, err
		}
		u.Label = fmt.Sprintf("USR-%d", u.ID)
		if last.Valid {
			u.LastActivity = &last.Time
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserActivity returns a user's request history, newest first.
func (s *Store) UserActivity(ctx context.Context, userID int64, limit int) ([]metrics.Entry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT "+entryCols+" FROM request_logs WHERE user_id=$1 ORDER BY ts DESC, id DESC LIMIT $2", userID, limit)
	if err != nil {
		return nil, err
	}
	return scanEntries(rows)
}

type OrderRow struct {
	OrderID   string    `json:"order_id"`
	UserID    int64     `json:"user_id"`
	User      string    `json:"user"`
	Items     string    `json:"items"`
	Quantity  int       `json:"quantity"`
	Total     float64   `json:"amount"`
	Status    string    `json:"status"`
	RequestID string    `json:"request_id"`
	Backend   string    `json:"backend_id"`
	CreatedAt time.Time `json:"created_at"`
}

const orderSelect = `SELECT o.order_number, o.user_id, u.name,
	COALESCE((SELECT string_agg(oi.product_name || ' x' || oi.quantity, ', ' ORDER BY oi.id) FROM order_items oi WHERE oi.order_id=o.id),''),
	COALESCE((SELECT sum(oi.quantity) FROM order_items oi WHERE oi.order_id=o.id),0)::int,
	o.total::float8, o.status, o.request_id, o.backend_id, o.created_at
	FROM orders o JOIN users u ON u.id=o.user_id`

func scanOrders(rows *sql.Rows) ([]OrderRow, error) {
	defer rows.Close()
	out := []OrderRow{}
	for rows.Next() {
		var o OrderRow
		if err := rows.Scan(&o.OrderID, &o.UserID, &o.User, &o.Items, &o.Quantity, &o.Total, &o.Status, &o.RequestID, &o.Backend, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) Orders(ctx context.Context, q string) ([]OrderRow, error) {
	args := []any{}
	where := "TRUE"
	if q = strings.TrimSpace(q); q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		where = `(o.order_number ILIKE $1 OR u.name ILIKE $1 OR o.request_id ILIKE $1 OR o.backend_id ILIKE $1 OR ('USR-' || u.id) ILIKE $1
		   OR EXISTS (SELECT 1 FROM order_items oi WHERE oi.order_id=o.id AND oi.product_name ILIKE $1))`
	}
	rows, err := s.DB.QueryContext(ctx, orderSelect+" WHERE "+where+" ORDER BY o.created_at DESC LIMIT 200", args...)
	if err != nil {
		return nil, err
	}
	return scanOrders(rows)
}

type Trace struct {
	Order    OrderRow         `json:"order"`
	Address  string           `json:"shipping_address"`
	Payment  string           `json:"payment_status"`
	Request  *metrics.Entry   `json:"order_request"`
	Timeline []metrics.Entry  `json:"timeline"`
	Database string           `json:"database"`
	Items    []map[string]any `json:"line_items"`
}

// OrderTrace reconstructs the journey behind one order: the user's requests leading up to it, the exact
// request that created it, the backend that served it and the database result.
func (s *Store) OrderTrace(ctx context.Context, number string) (*Trace, error) {
	rows, err := s.DB.QueryContext(ctx, orderSelect+" WHERE o.order_number=$1", number)
	if err != nil {
		return nil, err
	}
	os, err := scanOrders(rows)
	if err != nil {
		return nil, err
	}
	if len(os) == 0 {
		return nil, sql.ErrNoRows
	}
	t := &Trace{Order: os[0], Database: "SUCCESS", Timeline: []metrics.Entry{}, Items: []map[string]any{}}
	_ = s.DB.QueryRowContext(ctx, `SELECT shipping_address, payment_status FROM orders WHERE order_number=$1`, number).Scan(&t.Address, &t.Payment)
	irows, err := s.DB.QueryContext(ctx, `SELECT oi.product_name, oi.unit_price::float8, oi.quantity FROM order_items oi JOIN orders o ON o.id=oi.order_id WHERE o.order_number=$1 ORDER BY oi.id`, number)
	if err == nil {
		for irows.Next() {
			var n string
			var p float64
			var q int
			if irows.Scan(&n, &p, &q) == nil {
				t.Items = append(t.Items, map[string]any{"product": n, "unit_price": p, "quantity": q})
			}
		}
		irows.Close()
	}
	if len(t.Items) == 0 {
		t.Database = "INCONSISTENT: order has no items"
	}
	rr, err := s.DB.QueryContext(ctx, "SELECT "+entryCols+" FROM request_logs WHERE request_id=$1 AND order_id=$2 ORDER BY ts LIMIT 1", t.Order.RequestID, number)
	if err == nil {
		if es, e2 := scanEntries(rr); e2 == nil && len(es) > 0 {
			t.Request = &es[0]
		}
	}
	from := t.Order.CreatedAt.Add(-30 * time.Minute)
	if t.Request != nil {
		tr, err := s.DB.QueryContext(ctx, `SELECT * FROM (SELECT `+entryCols+` FROM request_logs
			WHERE user_id=$1 AND ts >= $2 AND ts <= $3 AND NOT load_test ORDER BY ts DESC, id DESC LIMIT 100) x ORDER BY ts, id`,
			t.Order.UserID, from, t.Request.Time.Add(time.Second))
		if err == nil {
			if es, e2 := scanEntries(tr); e2 == nil {
				t.Timeline = es
			}
		}
	}
	return t, nil
}

type HealthEventRow struct {
	Time    time.Time `json:"time"`
	Backend string    `json:"backend_id"`
	State   string    `json:"state"`
	Error   string    `json:"error"`
}

func (s *Store) HealthTransitions(ctx context.Context, limit int) ([]HealthEventRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT checked_at, backend_id, state, error FROM health_checks WHERE transition ORDER BY checked_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HealthEventRow{}
	for rows.Next() {
		var h HealthEventRow
		if err := rows.Scan(&h.Time, &h.Backend, &h.State, &h.Error); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
