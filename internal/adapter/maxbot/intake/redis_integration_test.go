//go:build integration

package intake

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestRedisDedupStore(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	testcontainers.CleanupContainer(t, ctr)
	uri, err := ctr.ConnectionString(ctx)
	require.NoError(t, err)
	opts, err := redis.ParseURL(uri)
	require.NoError(t, err)
	client := redis.NewClient(opts)
	defer func() { _ = client.Close() }()

	store := NewRedisDedupStore(client)
	key := UpdateKey(msg)

	fresh, err := store.SetNX(ctx, key, time.Hour)
	require.NoError(t, err)
	assert.True(t, fresh, "первое появление апдейта")

	fresh, err = store.SetNX(ctx, key, time.Hour)
	require.NoError(t, err)
	assert.False(t, fresh, "повторная доставка — дубликат")

	ttl, err := client.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.InDelta(t, time.Hour.Milliseconds(), ttl.Milliseconds(), 2000, "ключ живёт UPDATE_DEDUP_TTL")

	// После истечения TTL тот же апдейт снова считается новым.
	require.NoError(t, client.PExpireAt(ctx, key, time.Now().Add(-time.Second)).Err())
	fresh, err = store.SetNX(ctx, key, time.Hour)
	require.NoError(t, err)
	assert.True(t, fresh)
}
