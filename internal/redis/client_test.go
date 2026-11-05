package redis

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// A Redis that accepts connections and never answers is the failure that a dead
// port does not show. Without bounds, one command takes well over a minute.
func TestHungRedisFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // held open, never answered
		}
	}()

	client := redis.NewClient(options(ln.Addr().String()))
	defer client.Close()

	start := time.Now()
	err = client.ZCard(context.Background(), "queue:x").Err()
	if err == nil {
		t.Fatal("command against a silent server succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("failed after %v, want under 3s", elapsed)
	}
}
