package intake

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisDedupStore — антидубль на Redis: SET key 1 NX PX.
type RedisDedupStore struct {
	client *redis.Client
}

func NewRedisDedupStore(client *redis.Client) RedisDedupStore {
	return RedisDedupStore{client: client}
}

func (s RedisDedupStore) SetNX(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, key, 1, ttl).Result()
}
