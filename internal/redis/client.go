package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewClient creates a Redis client and verifies connectivity with a Ping.
func NewClient(ctx context.Context, addr string) (*redis.Client, error) {
	client := redis.NewClient(options(addr))
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("ping redis at startup (is redis up and REDIS_ADDR correct?): %w", err)
	}
	return client, nil
}

// options bounds how long an unreachable Redis can hold a caller. The expensive
// default is the dial path: 5s dials, retried 5 times, across 4 attempts took 1m41s
// against an address that never answers. Read and write keep the library's 3s
// default on purpose: a 500ms limit failed purchases when load saturated the host,
// while a healthy Redis was merely being scheduled late.
func options(addr string) *redis.Options {
	return &redis.Options{
		Addr:          addr,
		DialTimeout:   time.Second,
		MaxRetries:    1,
		DialerRetries: 1,
	}
}
