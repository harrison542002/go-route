package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/adapters/repositories/redisquota"
	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/drivers/redis"
	"github.com/harrison542002/go-route/internal/ports"
	"github.com/harrison542002/go-route/internal/usecases/quota"
)

const quotaCloseTimeout = 5 * time.Second

func buildQuota(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, table ports.PricingTable) (ports.QuotaEnforcer, func() error) {
	if !cfg.Redis.Enabled() {
		slog.Warn("QUOTAS ARE NOT ENFORCED: redis.addr is not set, so quota rows in Postgres are ignored")
		return nil, func() error { return nil }
	}

	client := redis.New(redis.Config{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		Timeout:  cfg.Redis.Timeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, cfg.Redis.Timeout)
	defer cancel()
	if err := redis.Ping(pingCtx, client); err != nil {
		slog.Error("QUOTA STORE UNREACHABLE AT STARTUP; requests will fail "+cfg.Redis.OnUnavailable+" until it answers",
			"addr", cfg.Redis.Addr, "err", err)
	} else {
		slog.Info("quota enforcement enabled",
			"redis", cfg.Redis.Addr, "on_unavailable", cfg.Redis.OnUnavailable)
	}

	if table == nil {
		slog.Error("QUOTAS CANNOT BE ENFORCED: no prices are configured, so no request can be costed; "+
			"set pricing.table or every request will be handled as "+cfg.Redis.OnUnavailable,
			"config_key", "pricing.table", "on_unavailable", cfg.Redis.OnUnavailable)
	}

	admin := repositories.NewAdminRepo(pool)

	//nolint:contextcheck // the delta flusher is deliberately detached; it outlives every request
	enforcer := quota.New(
		admin.Tenants(),
		admin.Quotas(),
		repositories.NewUsageCounterRepo(pool),
		redisquota.New(client, cfg.Redis.Timeout),
		table,
		quota.Config{
			OnUnavailable:    quota.OnUnavailable(cfg.Redis.OnUnavailable),
			DefaultMaxOutput: cfg.Quota.DefaultMaxOutputTokens,
			LimitsTTL:        cfg.Quota.LimitsTTL,
			FlushInterval:    cfg.Quota.FlushInterval,
			FlushBuffer:      cfg.Quota.FlushBuffer,
			ResyncInterval:   cfg.Quota.ResyncInterval,
		},
		time.Now,
	)

	// Close runs at shutdown, after every request context is gone.
	//
	//nolint:contextcheck // the final flush is bounded by its own timeout
	return enforcer, func() error {
		flushCtx, cancel := context.WithTimeout(context.Background(), quotaCloseTimeout)
		defer cancel()

		err := enforcer.Close(flushCtx)
		if cerr := client.Close(); err == nil {
			err = cerr
		}
		return err
	}
}
