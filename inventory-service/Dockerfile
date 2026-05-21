FROM golang:1.26 AS builder
WORKDIR /app

COPY inventory-service/go.mod .
COPY inventory-service/main.go .

RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o /inventory-service .

FROM alpine:latest
COPY --from=builder /inventory-service /inventory-service
CMD ["/inventory-service"]
