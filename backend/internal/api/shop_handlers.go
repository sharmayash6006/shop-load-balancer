package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"shop/backend/internal/database"
	"shop/backend/internal/models"
)

// ---------- products ----------

func (s *Server) listProducts(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cat := strings.TrimSpace(r.URL.Query().Get("category"))
	if len(q) > 100 || len(cat) > 100 {
		writeErr(w, 400, "query too long")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(),
		`SELECT id, name, description, price::float8, stock, category, image, created_at FROM products
		  WHERE ($1 = '' OR category = $1) AND ($2 = '' OR name ILIKE '%' || $2 || '%')
		  ORDER BY id`, cat, q)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	defer rows.Close()
	out := []models.Product{}
	for rows.Next() {
		var p models.Product
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.Image, &p.CreatedAt); err != nil {
			s.dbFail(w, r, err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) getProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "invalid product id")
		return
	}
	var p models.Product
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT id, name, description, price::float8, stock, category, image, created_at FROM products WHERE id=$1`, id).
		Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.Image, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "product not found")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	meta(w, "Product", p.Name)
	writeJSON(w, 200, p)
}

// ---------- cart ----------

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func loadCart(ctx context.Context, db queryer, userID int64) (models.Cart, error) {
	c := models.Cart{Items: []models.CartItem{}}
	rows, err := db.QueryContext(ctx,
		`SELECT p.id, p.name, p.price::float8, p.image, p.stock, ci.quantity
		   FROM cart c JOIN cart_items ci ON ci.cart_id = c.id JOIN products p ON p.id = ci.product_id
		  WHERE c.user_id = $1 ORDER BY ci.added_at, p.id`, userID)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var it models.CartItem
		if err := rows.Scan(&it.ProductID, &it.Name, &it.Price, &it.Image, &it.Stock, &it.Quantity); err != nil {
			return c, err
		}
		it.LineTotal = it.Price * float64(it.Quantity)
		c.Count += it.Quantity
		c.Total += it.LineTotal
		c.Items = append(c.Items, it)
	}
	return c, rows.Err()
}

func (s *Server) getCart(w http.ResponseWriter, r *http.Request, p Principal) {
	c, err := loadCart(r.Context(), s.DB, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	meta(w, "Quantity", strconv.Itoa(c.Count))
	writeJSON(w, 200, c)
}

func (s *Server) productName(ctx context.Context, id int64) string {
	var n string
	_ = s.DB.QueryRowContext(ctx, `SELECT name FROM products WHERE id=$1`, id).Scan(&n)
	return n
}

func (s *Server) addToCart(w http.ResponseWriter, r *http.Request, p Principal) {
	var in struct {
		ProductID int64 `json:"product_id"`
		Quantity  int   `json:"quantity"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Quantity == 0 {
		in.Quantity = 1
	}
	if in.ProductID <= 0 || in.Quantity < 1 || in.Quantity > 99 {
		writeErr(w, 400, "product_id and quantity (1-99) are required")
		return
	}
	var cartID int64
	err := s.DB.QueryRowContext(r.Context(),
		`INSERT INTO cart (user_id) VALUES ($1) ON CONFLICT (user_id) DO UPDATE SET updated_at = now() RETURNING id`, p.ID).Scan(&cartID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	_, err = s.DB.ExecContext(r.Context(),
		`INSERT INTO cart_items (cart_id, product_id, quantity) VALUES ($1,$2,$3)
		 ON CONFLICT (cart_id, product_id) DO UPDATE SET quantity = LEAST(99, cart_items.quantity + EXCLUDED.quantity)`,
		cartID, in.ProductID, in.Quantity)
	if database.IsForeignKeyViolation(err) {
		writeErr(w, 404, "product not found")
		return
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	meta(w, "Product", s.productName(r.Context(), in.ProductID))
	meta(w, "Quantity", strconv.Itoa(in.Quantity))
	c, err := loadCart(r.Context(), s.DB, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) setCartQuantity(w http.ResponseWriter, r *http.Request, p Principal) {
	pid, ok := pathID(r)
	var in struct {
		Quantity int `json:"quantity"`
	}
	if !ok || !decode(w, r, &in) {
		if !ok {
			writeErr(w, 400, "invalid product id")
		}
		return
	}
	if in.Quantity < 0 || in.Quantity > 99 {
		writeErr(w, 400, "quantity must be 0-99")
		return
	}
	var res sql.Result
	var err error
	if in.Quantity == 0 {
		res, err = s.DB.ExecContext(r.Context(),
			`DELETE FROM cart_items WHERE product_id=$2 AND cart_id=(SELECT id FROM cart WHERE user_id=$1)`, p.ID, pid)
	} else {
		res, err = s.DB.ExecContext(r.Context(),
			`UPDATE cart_items SET quantity=$3 WHERE product_id=$2 AND cart_id=(SELECT id FROM cart WHERE user_id=$1)`, p.ID, pid, in.Quantity)
	}
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "item not in cart")
		return
	}
	meta(w, "Product", s.productName(r.Context(), pid))
	meta(w, "Quantity", strconv.Itoa(in.Quantity))
	c, err := loadCart(r.Context(), s.DB, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) removeCartItem(w http.ResponseWriter, r *http.Request, p Principal) {
	pid, ok := pathID(r)
	if !ok {
		writeErr(w, 400, "invalid product id")
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM cart_items WHERE product_id=$2 AND cart_id=(SELECT id FROM cart WHERE user_id=$1)`, p.ID, pid)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, 404, "item not in cart")
		return
	}
	meta(w, "Product", s.productName(r.Context(), pid))
	c, err := loadCart(r.Context(), s.DB, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) clearCart(w http.ResponseWriter, r *http.Request, p Principal) {
	_, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM cart_items WHERE cart_id=(SELECT id FROM cart WHERE user_id=$1)`, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	writeJSON(w, 200, models.Cart{Items: []models.CartItem{}})
}

// checkout validates the cart and returns the order summary the user confirms before placing the order.
func (s *Server) checkout(w http.ResponseWriter, r *http.Request, p Principal) {
	c, err := loadCart(r.Context(), s.DB, p.ID)
	if err != nil {
		s.dbFail(w, r, err)
		return
	}
	if len(c.Items) == 0 {
		writeErr(w, 400, "cart is empty")
		return
	}
	for _, it := range c.Items {
		if it.Stock < it.Quantity {
			writeErr(w, 409, "insufficient stock for "+it.Name)
			return
		}
	}
	meta(w, "Quantity", strconv.Itoa(c.Count))
	meta(w, "Amount", strconv.FormatFloat(c.Total, 'f', 2, 64))
	writeJSON(w, 200, map[string]any{"cart": c, "total": c.Total, "currency": "INR"})
}
