package httpserver

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// RedisPinger адаптирует клиент Redis к Pinger для /readyz.
type RedisPinger struct {
	client *redis.Client
}

func NewRedisPinger(client *redis.Client) RedisPinger {
	return RedisPinger{client: client}
}

func (p RedisPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}
