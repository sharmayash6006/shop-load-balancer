package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"shop/backend/internal/database"
	"shop/backend/internal/middleware"
	"shop/backend/internal/models"
)

type orderLine struct {
	ProductID int64 `json:"product_id"`
	Quantity  int   `json:"quantity"`
}

type stockError struct{ product string }

func (e stockError) Error() string { return "insufficient stock for " + e.product }

var errEmptyOrder = errors.New("no items to order")

// createOrder places an order inside ONE database transaction:
//  1. look up an existing order for (user, Idempotency-Key) -> replay it, never duplicate
//  2. lock product rows (in id order, to avoid deadlocks), check and decrement stock
//  3. insert order + order_items, simulate payment (PAID), clear the cart
//
// Either everything commits or nothing does.
func (s *Server) createOrder(w http.ResponseWriter, r *http.Request, p Principal) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 100 {
		writeErr(w, 400, "Idempotency-Key header is required (max 100 characters)")
		return
	}
	var in struct {
		Address string      `json:"address"`
		Items   []orderLine `json:"items"` // optional "buy now"; when empty the cart is used
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	if utf8.RuneCountInString(in.Address) > 300 {
		writeErr(w, 400, "address too long")
		return
	}
	if len(in.Items) > 50 {
		writeErr(w, 400, "too many items")
		return
	}
	for _, it := range in.Items {
		if it.ProductID <= 0 || it.Quantity < 1 || it.Quantity > 99 {
			writeErr(w, 400, "invalid item")
			return
		}
	}

	reqID := middleware.RequestID(r.Context())
	order, replay, err := s.placeOrder(r.Context(), p, key, in.Address, in.Items, reqID)
	if err != nil && database.IsUniqueViolation(err) {
		// A concurrent request with the same key won the race: return its order.
		order, err = s.findOrder(r.Context(), s.DB, p.ID, "", key)
		replay = true
	}
	var se stockError
	switch {
	case errors.As(err, &se):
		writeErr(w, 409, se.Error())
		return
	case errors.Is(err, errEmptyOrder):
		writeErr(w, 400, "cart is empty")
		return
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, 404, "product not found")
		return
	case err != nil:
		s.dbFail(w, r, err)
		return
	}
	meta(w, "Order-ID", order.ID)
	meta(w, "Amount", strconv.FormatFloat(order.Total, 'f', 2, 64))
	if len(order.Items) > 0 {
		meta(w, "Product", order.Items[0].ProductName)
		q := 0
		for _, it := range order.Items {
			q += it.Quantity
		}
		meta(w, "Quantity", strconv.Itoa(q))
	}
	code := 201
	if replay {
		code = 200
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, code, order)
}

func (s *Server) placeOrder(ctx context.Context, p Principal, key, address string, lines []orderLine, reqID string) (models.Order, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return models.Order{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	if existing, err := s.findOrder(ctx, tx, p.ID, "", key); err == nil {
		return existing, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return models.Order{}, false, err
	}

	fromCart := len(lines) == 0
	if fromCart {
		rows, err := tx.QueryContext(ctx,
			`SELECT ci.product_id, ci.quantity FROM cart c JOIN cart_items ci ON ci.cart_id=c.id WHERE c.user_id=$1`, p.ID)
		if err != nil {
			return models.Order{}, false, err
		}
		for rows.Next() {
			var l orderLine
			if err := rows.Scan(&l.ProductID, &l.Quantity); err != nil {
				rows.Close()
				return models.Order{}, false, err
			}
			lines = append(lines, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return models.Order{}, false, err
		}
	}
	if len(lines) == 0 {
		return models.Order{}, false, errEmptyOrder
	}
	// merge duplicates, sort by product id (consistent lock order)
	merged := map[int64]int{}
	for _, l := range lines {
		merged[l.ProductID] += l.Quantity
	}
	ids := make([]int64, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	o := models.Order{Status: "CONFIRMED", PaymentStatus: "PAID", ShippingAddress: address, RequestID: reqID, BackendID: s.BackendID}
	for _, id := range ids {
		qty := merged[id]
		var name string
		var price float64
		var stock int
		if err := tx.QueryRowContext(ctx, `SELECT name, price::float8, stock FROM products WHERE id=$1 FOR UPDATE`, id).Scan(&name, &price, &stock); err != nil {
			return models.Order{}, false, err
		}
		if stock < qty {
			return models.Order{}, false, stockError{name}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE products SET stock = stock - $2 WHERE id=$1`, id, qty); err != nil {
			return models.Order{}, false, err
		}
		o.Items = append(o.Items, models.OrderItem{ProductID: id, ProductName: name, UnitPrice: price, Quantity: qty})
		o.Total += price * float64(qty)
	}

	var orderID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO orders (order_number, user_id, status, payment_status, total, shipping_address, request_id, backend_id, idempotency_key)
		 VALUES ('ORD-' || nextval('order_seq'), $1, $2, $3, $4, $5, $6, $7, $8) RETURNING id, order_number, created_at`,
		p.ID, o.Status, o.PaymentStatus, o.Total, o.ShippingAddress, o.RequestID, o.BackendID, key).
		Scan(&orderID, &o.ID, &o.CreatedAt)
	if err != nil {
		return models.Order{}, false, err
	}
	for _, it := range o.Items {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO order_items (order_id, product_id, product_name, unit_price, quantity) VALUES ($1,$2,$3,$4,$5)`,
			orderID, it.ProductID, it.ProductName, it.UnitPrice, it.Quantity); err != nil {
			return models.Order{}, false, err
		}
	}
	if fromCart {
		if _, err := tx.ExecContext(ctx, `DELETE FROM cart_items WHERE cart_id=(SELECT id FROM cart WHERE user_id=$1)`, p.ID); err != nil {
			return models.Order{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return models.Order{}, false, err
	}
	return o, false, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// findOrder loads an order (with items) by idempotency key or by order number, for one user.
func (s *Server) findOrder(ctx context.Context, q rowQuerier, userID int64, number, key string) (models.Order, error) {
	var o models.Order
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT id, order_number, status, payment_status, total::float8, shipping_address, request_id, backend_id, created_at
		   FROM orders WHERE user_id=$1 AND (($2 <> '' AND order_number=$2) OR ($3 <> '' AND idempotency_key=$3))`,
		userID, number, key).
		Scan(&id, &o.ID, &o.Status, &o.PaymentStatus, &o.Total, &o.ShippingAddress, &o.RequestID, &o.BackendID, &o.CreatedAt)
	if err != nil {
		return o, err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT product_id, product_name, unit_price::float8, quantity FROM order_items WHERE order_id=$1 ORDER BY id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	o.Items = []models.OrderItem{}
	for rows.Next() {
		var it models.OrderItem
		if err := rows.Scan(&it.ProductID, &it.ProductName, &it.UnitPrice, &it.Quantity); err != nil {
			return o, err
		}
		o.Items = append(o.Items, it)
	}
	return o, rows.Err()
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request, p Principal) {
	num := r.PathValue("id")
	if len(num) > 40 {
		writeErr(w, 400, "invalid order id")
		return
	}
	o, err := s.findOrder(r.Context(), s.DB, p.ID, num, "")
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "order not found")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	meta(w, "Order-ID", o.ID)
	writeJSON(w, 200, o)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request, p Principal) {
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT o.id, o.order_number, o.status, o.payment_status, o.total::float8, o.shipping_address, o.request_id, o.backend_id, o.created_at
		   FROM orders o WHERE o.user_id=$1 ORDER BY o.created_at DESC LIMIT 100`, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	defer rows.Close()
	out := []models.Order{}
	ids := []int64{}
	idx := map[int64]int{}
	for rows.Next() {
		var o models.Order
		var id int64
		if err := rows.Scan(&id, &o.ID, &o.Status, &o.PaymentStatus, &o.Total, &o.ShippingAddress, &o.RequestID, &o.BackendID, &o.CreatedAt); err != nil {
			s.dbFail(w, r, err)
			return
		}
		o.Items = []models.OrderItem{}
		idx[id] = len(out)
		ids = append(ids, id)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		s.dbFail(w, r, err)
		return
	}
	rows.Close()
	for _, id := range ids {
		irows, err := s.DB.QueryContext(r.Context(),
			`SELECT product_id, product_name, unit_price::float8, quantity FROM order_items WHERE order_id=$1 ORDER BY id`, id)
		if err != nil {
			s.dbFail(w, r, err)
			return
		}
		for irows.Next() {
			var it models.OrderItem
			if err := irows.Scan(&it.ProductID, &it.ProductName, &it.UnitPrice, &it.Quantity); err != nil {
				irows.Close()
				s.dbFail(w, r, err)
				return
			}
			out[idx[id]].Items = append(out[idx[id]].Items, it)
		}
		irows.Close()
	}
	writeJSON(w, 200, out)
}
