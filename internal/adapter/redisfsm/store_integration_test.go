//go:build integration

// Интеграционные тесты FSMStore на реальном Redis.
package redisfsm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

var testClient *redis.Client

func TestMain(m *testing.M) {
	os.Exit(runWithRedis(m))
}

func runWithRedis(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintln(os.Stderr, "запуск контейнера Redis:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "строка подключения:", err)
		return 1
	}
	opts, err := redis.ParseURL(uri)
	if err != nil {
		fmt.Fprintln(os.Stderr, "адрес Redis:", err)
		return 1
	}
	testClient = redis.NewClient(opts)
	defer func() { _ = testClient.Close() }()
	return m.Run()
}

func TestStore_SetGetDelete(t *testing.T) {
	ctx := context.Background()
	s := New(testClient)
	key := "fsm:" + t.Name()

	_, found, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, found, "нет ключа — не ошибка")

	require.NoError(t, s.Set(ctx, key, `{"state":"entering_text"}`, time.Minute))
	v, found, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.True(t, found)
	assert.JSONEq(t, `{"state":"entering_text"}`, v)

	require.NoError(t, s.Delete(ctx, key))
	_, found, err = s.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, found)

	assert.NoError(t, s.Delete(ctx, key), "удаление отсутствующего ключа — не ошибка")
}

func TestStore_TTLSetAndExtended(t *testing.T) {
	ctx := context.Background()
	s := New(testClient)
	key := "fsm:" + t.Name()

	require.NoError(t, s.Set(ctx, key, "v1", time.Minute))
	ttl, err := testClient.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.InDelta(t, time.Minute.Milliseconds(), ttl.Milliseconds(), 2000, "TTL ставится")

	// Каждый шаг диалога продлевает TTL (раздел 8.2).
	require.NoError(t, s.Set(ctx, key, "v2", 30*time.Minute))
	ttl, err = testClient.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.InDelta(t, (30 * time.Minute).Milliseconds(), ttl.Milliseconds(), 2000, "перезапись продлевает TTL")
}

func TestStore_ExpiredKeyIsNotFound(t *testing.T) {
	ctx := context.Background()
	s := New(testClient)
	key := "fsm:" + t.Name()
	require.NoError(t, s.Set(ctx, key, "v", time.Hour))

	// Истечение TTL без ожидания: срок в прошлом Redis удаляет ключ сразу.
	require.NoError(t, testClient.PExpireAt(ctx, key, time.Now().Add(-time.Second)).Err())

	_, found, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.False(t, found, "истёкший черновик равносилен отсутствующему")
}

func TestStore_RedisUnavailable(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	defer func() { _ = down.Close() }()
	s := New(down)
	ctx := context.Background()

	_, _, err := s.Get(ctx, "k")
	require.Error(t, err, "недоступный Redis — ошибка, а не «ключа нет»")
	assert.Error(t, s.Set(ctx, "k", "v", time.Minute))
}
