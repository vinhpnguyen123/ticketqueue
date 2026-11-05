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

// options bounds every command. Redis is the fast path and every caller has an
// answer for it being unavailable, so a hung Redis must fail quickly: with the
// library defaults one command took 1m41s, past the ALB's 60s idle timeout, and
// promotion would pay that once per admission. A healthy command takes under a
// millisecond, so 500ms is far above normal load.
func options(addr string) *redis.Options {
	return &redis.Options{
		Addr:          addr,
		DialTimeout:   500 * time.Millisecond,
		ReadTimeout:   500 * time.Millisecond,
		WriteTimeout:  500 * time.Millisecond,
		MaxRetries:    1,
		DialerRetries: 1,
	}
}
