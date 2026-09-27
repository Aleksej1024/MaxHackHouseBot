// Package redisfsm реализует usecase.FSMStore на Redis:
// временное состояние диалогов и сессий с TTL.
package redisfsm

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store реализует usecase.FSMStore.
type Store struct {
	client *redis.Client
}

func New(client *redis.Client) *Store {
	return &Store{client: client}
}

// Get возвращает значение; found = false, если ключа нет или истёк TTL.
func (s *Store) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Set записывает значение с TTL (перезапись продлевает TTL).
func (s *Store) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}
