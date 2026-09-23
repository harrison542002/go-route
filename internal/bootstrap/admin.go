package bootstrap

import (
	"context"
	"net/http"
	"time"

	"github.com/harrison542002/go-route/internal/adapters/inbound/adminapi"
	"github.com/harrison542002/go-route/internal/adapters/repositories"
	"github.com/harrison542002/go-route/internal/core/tokens"
	"github.com/harrison542002/go-route/internal/drivers/postgresql"
	"github.com/harrison542002/go-route/internal/usecases/admin"
)

const (
	DefaultAdminListen         = "127.0.0.1:4001"
	DefaultAdminRequestTimeout = 10 * time.Second
	MaxAdminRequestTimeout     = 2 * time.Minute
	DefaultAccessTokenTTL      = 15 * time.Minute
	AccessTokenIssuer          = "go-route-admin"
)

// AdminOptions is everything the admin service needs to exist. It deliberately
// does not read the proxy's YAML, none of which this service needs.
type AdminOptions struct {
	DSN            string
	Listen         string
	RequestTimeout time.Duration
	JWTSecret      []byte
	AccessTokenTTL time.Duration
}

// AdminApp is the admin service, assembled. Close releases what BuildAdmin
// opened, and is safe to call once.
type AdminApp struct {
	Server *http.Server
	Close  func() error
}

// BuildAdmin composes the admin service: its own pool, its own partition
// maintenance and its own listener.
func BuildAdmin(ctx context.Context, opts AdminOptions) (*AdminApp, error) {
	pool, err := postgresql.Connect(ctx, opts.DSN)
	if err != nil {
		return nil, err
	}

	partitions := postgresql.NewPartitions(pool)
	if err := partitions.Ensure(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	//nolint:contextcheck // maintenance outlives any caller's context by design
	partitions.Start()

	issuer, err := tokens.NewIssuer(opts.JWTSecret, AccessTokenIssuer, opts.AccessTokenTTL, time.Now)
	if err != nil {
		_ = partitions.Stop()
		pool.Close()
		return nil, err
	}

	creds := repositories.NewAdminCredentialRepo(pool)
	userRepo := repositories.NewAdminUserRepo(pool)
	users := admin.NewUsers(userRepo, issuer, admin.DefaultRefreshTTL, time.Now, nil)

	records := repositories.NewObservabilityRepoWithPool(pool)

	server, err := adminapi.NewServer(
		opts.Listen,
		adminapi.NewHandler(
			admin.New(repositories.NewAdminRepo(pool), time.Now, nil), users, records, time.Now),
		creds,
		userRepo,
		issuer,
		opts.RequestTimeout,
	)
	if err != nil {
		_ = partitions.Stop()
		pool.Close()
		return nil, err
	}

	return &AdminApp{
		Server: server,
		Close: func() error {
			err := partitions.Stop()
			pool.Close()
			return err
		},
	}, nil
}
