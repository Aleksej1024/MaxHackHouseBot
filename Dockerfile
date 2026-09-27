# syntax=docker/dockerfile:1

FROM registry.altlinux.org/alt/alt:p11 AS builder
RUN apt-get update \
    && apt-get install -y golang ca-certificates \
    && apt-get clean && rm -rf /var/lib/apt/lists/*
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/bot ./cmd/bot

FROM registry.altlinux.org/alt/alt:p11
# ca-certificates-digital.gov.ru — корневой сертификат Минцифры: им подписан
# API MAX (platform-api2.max.ru). wget — для health-check в docker-compose.prod.yml.
RUN apt-get update \
    && apt-get install -y ca-certificates ca-certificates-digital.gov.ru wget \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    && useradd -M -r -u 10001 -s /sbin/nologin bot \
    && mkdir -p /var/log/bot && chown bot:bot /var/log/bot
USER bot
COPY --from=builder /bin/bot /bot
COPY migrations /migrations
EXPOSE 8080
ENTRYPOINT ["/bot"]
