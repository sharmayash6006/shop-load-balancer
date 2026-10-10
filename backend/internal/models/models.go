package models

import "time"

type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type Product struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Stock       int       `json:"stock"`
	Category    string    `json:"category"`
	Image       string    `json:"image"`
	CreatedAt   time.Time `json:"created_at"`
}

type CartItem struct {
	ProductID int64   `json:"product_id"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	Image     string  `json:"image"`
	Stock     int     `json:"stock"`
	Quantity  int     `json:"quantity"`
	LineTotal float64 `json:"line_total"`
}

type Cart struct {
	Items []CartItem `json:"items"`
	Count int        `json:"count"`
	Total float64    `json:"total"`
}

type OrderItem struct {
	ProductID   int64   `json:"product_id"`
	ProductName string  `json:"product_name"`
	UnitPrice   float64 `json:"unit_price"`
	Quantity    int     `json:"quantity"`
}

type Order struct {
	ID              string      `json:"id"` // ORD-10293
	Status          string      `json:"status"`
	PaymentStatus   string      `json:"payment_status"`
	Total           float64     `json:"total"`
	ShippingAddress string      `json:"shipping_address"`
	RequestID       string      `json:"request_id"`
	BackendID       string      `json:"backend_id"`
	CreatedAt       time.Time   `json:"created_at"`
	Items           []OrderItem `json:"items"`
}
