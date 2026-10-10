FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backend ./cmd/server

FROM alpine:3.20
RUN adduser -D -u 10001 app && apk add --no-cache wget
USER app
COPY --from=build /out/backend /backend
EXPOSE 8001
ENTRYPOINT ["/backend"]
