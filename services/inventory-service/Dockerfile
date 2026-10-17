FROM golang:1.26 AS builder
WORKDIR /app

COPY services/inventory-service/go.mod services/inventory-service/go.sum ./
COPY services/inventory-service/*.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o /inventory-service .

FROM alpine:3.24.2
COPY --from=builder /inventory-service /inventory-service
USER 65534:65534
CMD ["/inventory-service"]
