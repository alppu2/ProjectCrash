FROM golang:1.26 AS builder
WORKDIR /app

COPY order-service/go.mod .
COPY order-service/main.go .
COPY orders/ orders/

RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o /order-service .

FROM alpine:latest
COPY --from=builder /order-service /order-service
EXPOSE 50051
CMD ["/order-service"]
