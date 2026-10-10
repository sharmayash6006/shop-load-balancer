.PHONY: up down logs test-lb test-backend load
up:        ; docker compose up --build
down:      ; docker compose down
reset:     ; docker compose down -v      # also deletes the database volume
logs:      ; docker compose logs -f load-balancer
test-lb:   ; cd load-balancer && go mod tidy && go test ./...
test-backend: ; cd backend && go mod tidy && TEST_DATABASE_URL="postgres://shop:shop@localhost:5432/shop?sslmode=disable" go test ./...
load:      ; docker compose run --rm k6 run /tests/ecommerce-test.js
