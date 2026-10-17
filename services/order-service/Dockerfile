FROM golang:1.26 AS builder
WORKDIR /app

COPY services/order-service/go.mod services/order-service/go.sum ./
COPY services/order-service/*.go ./
COPY services/order-service/orders/ orders/

RUN CGO_ENABLED=0 GOOS=linux go build -o /order-service .

FROM alpine:3.24.2
COPY --from=builder /order-service /order-service
EXPOSE 50051
USER 65534:65534
CMD ["/order-service"]
