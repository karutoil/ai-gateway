package cache

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

type redisEntry struct {
	Body    []byte      `json:"body"`
	Status  int         `json:"status"`
	Headers http.Header `json:"headers"`
}

// RedisCache implements Cache via Redis when REDIS_URL is set.
//
// Redis failures degrade to cache misses (never to a process-local mirror):
// previously every write was ALSO mirrored into an instance-local memory
// cache, so after a Redis flap different replicas served different stale
// answers under the same key. Multi-instance consistency now follows Redis.
type RedisCache struct {
	client *redis.Client
}

var _ Cache = (*RedisCache)(nil)

// NewRedisCache creates a Redis-backed cache. It parses redisURL and attempts
// a Ping with 2s timeout (failures are logged but non-fatal: operations will
// simply miss until connectivity recovers).
func NewRedisCache(redisURL string) (*RedisCache, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Warn().Err(err).Str("redis_url", redisURL).Msg("redis ping failed, cache will serve misses until reachable")
	}
	return &RedisCache{client: client}, nil
}

// NewRedisCacheWithClient is for testing / wiring with an existing client.
func NewRedisCacheWithClient(client *redis.Client) *RedisCache {
	return &RedisCache{client: client}
}

func (r *RedisCache) Get(key string) ([]byte, int, http.Header, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	data, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		if err != redis.Nil {
			log.Debug().Err(err).Str("key", key).Msg("redis Get failed")
		}
		return nil, 0, nil, false
	}
	var e redisEntry
	if err := json.Unmarshal(data, &e); err != nil {
		log.Debug().Err(err).Msg("redis unmarshal failed")
		return nil, 0, nil, false
	}
	return e.Body, e.Status, e.Headers, true
}

func (r *RedisCache) Set(key string, body []byte, status int, headers http.Header, ttlSeconds int) {
	if ttlSeconds <= 0 {
		ttlSeconds = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	e := redisEntry{Body: body, Status: status, Headers: headers}
	data, err := json.Marshal(e)
	if err != nil {
		log.Debug().Err(err).Msg("redis marshal failed")
		return
	}
	if err := r.client.Set(ctx, key, data, time.Duration(ttlSeconds)*time.Second).Err(); err != nil {
		log.Debug().Err(err).Str("key", key).Msg("redis Set failed")
	}
}

func (r *RedisCache) Invalidate(pattern string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var match string
	if pattern == "*" {
		match = "*"
	} else {
		match = pattern + "*"
	}
	iter := r.client.Scan(ctx, 0, match, 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
		if len(keys) >= 200 {
			if err := r.client.Del(ctx, keys...).Err(); err != nil {
				log.Debug().Err(err).Msg("redis Del batch failed")
			}
			keys = keys[:0]
		}
	}
	if err := iter.Err(); err != nil {
		log.Debug().Err(err).Msg("redis Scan failed during Invalidate")
	}
	if len(keys) > 0 {
		_ = r.client.Del(ctx, keys...).Err()
	}
}

// Close closes the redis client.
func (r *RedisCache) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}
