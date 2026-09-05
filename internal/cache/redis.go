// Package cache provides caching implementations for link resolution.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mmuqiitf/url-shortener/internal/model"
)

// RedisCache is a Redis-backed distributed cache implementing the Cache interface.
type RedisCache struct {
	client *redis.Client
	prefix string
}

// NewRedisCache creates a new RedisCache client and verifies connectivity.
func NewRedisCache(ctx context.Context, addr, password string, db int) (*RedisCache, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}

	return &RedisCache{
		client: rdb,
		prefix: "link:",
	}, nil
}

// Get fetches a link from Redis and deserializes it from JSON.
func (r *RedisCache) Get(ctx context.Context, code string) (model.Link, bool) {
	data, err := r.client.Get(ctx, r.prefix+code).Bytes()
	if errors.Is(err, redis.Nil) {
		return model.Link{}, false
	}
	if err != nil {
		// If Redis has a transient network error, degrade gracefully to a cache miss
		return model.Link{}, false
	}

	var l model.Link
	if err := json.Unmarshal(data, &l); err != nil {
		return model.Link{}, false
	}
	return l, true
}

// Set serializes a link to JSON and stores it in Redis with the given TTL.
func (r *RedisCache) Set(ctx context.Context, link model.Link, ttl time.Duration) {
	data, err := json.Marshal(link)
	if err != nil {
		return
	}
	_ = r.client.Set(ctx, r.prefix+link.Code, data, ttl).Err()
}

// Delete removes a link from Redis.
func (r *RedisCache) Delete(ctx context.Context, code string) {
	_ = r.client.Del(ctx, r.prefix+code).Err()
}

// Close closes the Redis client pool.
func (r *RedisCache) Close() error {
	return r.client.Close()
}
