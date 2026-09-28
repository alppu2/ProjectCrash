FROM golang:1.26 AS builder
WORKDIR /app

COPY chat-service/go.mod chat-service/go.sum ./
COPY chat-service/*.go ./
COPY chat-service/chat/ chat/
# The *.go copy above is flat and would miss both subdirectories.
COPY chat-service/internal/ internal/
COPY chat-service/cmd/ cmd/

RUN CGO_ENABLED=0 GOOS=linux go build -o /chat-service . \
 && CGO_ENABLED=0 GOOS=linux go build -o /ingest ./cmd/ingest

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=builder /chat-service /chat-service
COPY --from=builder /ingest /ingest
EXPOSE 50051
CMD ["/chat-service"]
