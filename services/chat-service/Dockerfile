FROM golang:1.26 AS builder
WORKDIR /app

COPY services/chat-service/go.mod services/chat-service/go.sum ./
COPY services/chat-service/chat/ chat/
COPY services/chat-service/internal/ internal/
COPY services/chat-service/cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux go build -o /chat-service ./cmd/chat-service \
 && CGO_ENABLED=0 GOOS=linux go build -o /ingest ./cmd/ingest

FROM alpine:3.24.2
RUN apk add --no-cache ca-certificates
COPY --from=builder /chat-service /chat-service
COPY --from=builder /ingest /ingest
EXPOSE 50051
USER 65534:65534
CMD ["/chat-service"]
