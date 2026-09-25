package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type Config struct {
	Addr     string
	Password string
	DB       int
	Timeout  time.Duration
}

// New dials nothing; go-redis connects lazily. Retries are disabled on purpose:
// the reserve script is not idempotent, so a retry after a reply lost in
// transit would reserve twice.
func New(cfg Config) *goredis.Client {
	return goredis.NewClient(&goredis.Options{
		Addr:                  cfg.Addr,
		Password:              cfg.Password,
		DB:                    cfg.DB,
		DialTimeout:           cfg.Timeout,
		ReadTimeout:           cfg.Timeout,
		WriteTimeout:          cfg.Timeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
		DialerRetries:         1,
	})
}

// Ping is separate from New because an unreachable Redis is not a startup
// failure here: the caller decides what to do about it.
func Ping(ctx context.Context, client *goredis.Client) error {
	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis: ping: %w", err)
	}
	return nil
}
