# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/bot ./cmd/bot

FROM alpine:3.21
RUN adduser -D -u 10001 bot \
    && mkdir -p /var/log/bot && chown bot:bot /var/log/bot
USER bot
COPY --from=builder /bin/bot /bot
COPY migrations /migrations
EXPOSE 8080
ENTRYPOINT ["/bot"]
