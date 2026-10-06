FROM golang:1.26 AS builder
WORKDIR /app

COPY services/order-service/go.mod services/order-service/go.sum ./
COPY services/order-service/*.go ./
COPY services/order-service/orders/ orders/

RUN CGO_ENABLED=0 GOOS=linux go build -o /order-service .

FROM alpine:latest
COPY --from=builder /order-service /order-service
EXPOSE 50051
CMD ["/order-service"]
