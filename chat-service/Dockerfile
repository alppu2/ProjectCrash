FROM golang:1.26 AS builder
WORKDIR /app

COPY chat-service/go.mod chat-service/go.sum ./
COPY chat-service/*.go ./
COPY chat-service/chat/ chat/

RUN CGO_ENABLED=0 GOOS=linux go build -o /chat-service .

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /chat-service /chat-service
EXPOSE 50051
CMD ["/chat-service"]
