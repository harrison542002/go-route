package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/bootstrap"
	"github.com/harrison542002/go-route/internal/core/tokens"
)

const drainTimeout = 25 * time.Second

func serveCmd() *cobra.Command {
	var (
		listen         string
		requestTimeout time.Duration
		jwtSecret      string
		accessTokenTTL time.Duration
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the admin API",
		Long: `Serves the admin API on --listen. It authenticates every call,
either against admin_credentials for a machine token or against a
signed access token for a person, so a deployment with no credential
minted and no user created answers 401 to everything; make the first
one with create-token or create-user.

--jwt-secret signs the access tokens people get when they log in, and
is required: a secret this process invented would be different after
every restart, so every session would end with a deployment. Pass it,
or set GO_ROUTE_JWT_SECRET, with at least 32 bytes of it.

Settings come from flags, falling back to the environment for the DSN
and the signing secret: --dsn then $DATABASE_URL, --jwt-secret then
$GO_ROUTE_JWT_SECRET. There is no config file.`,
		Args: cobra.NoArgs,
		Example: `  go-route-admin serve --listen 127.0.0.1:4001
  GO_ROUTE_JWT_SECRET=$(openssl rand -base64 48) go-route-admin serve`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(listen, requestTimeout, jwtSecret, accessTokenTTL)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", bootstrap.DefaultAdminListen,
		"address to serve the admin API on")
	cmd.Flags().DurationVar(&requestTimeout, "request-timeout", bootstrap.DefaultAdminRequestTimeout,
		"deadline for each admin request, database work included")
	cmd.Flags().StringVar(&jwtSecret, "jwt-secret", "",
		"HMAC secret signing access tokens, at least 32 bytes (defaults to $GO_ROUTE_JWT_SECRET)")
	cmd.Flags().DurationVar(&accessTokenTTL, "access-token-ttl", bootstrap.DefaultAccessTokenTTL,
		"how long an access token verifies; the client refreshes when it expires")
	return cmd
}

func runServe(listen string, requestTimeout time.Duration, jwtSecret string, accessTokenTTL time.Duration) error {
	opts, err := serveOptions(listen, requestTimeout, jwtSecret, accessTokenTTL)
	if err != nil {
		return err
	}

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelBuild()

	application, err := bootstrap.BuildAdmin(buildCtx, opts)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("admin api listening", "addr", opts.Listen, "request_timeout", opts.RequestTimeout)
		if err := application.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		_ = application.Server.Close()
		_ = application.Close()
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "drain_timeout", drainTimeout)
	}

	stop()

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelDrain()

	if err := application.Server.Shutdown(drainCtx); errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("drain window expired; cutting remaining requests")
	}

	if err := application.Close(); err != nil {
		slog.Error("close failed", "err", err)
	}

	slog.Info("shutdown complete")
	return nil
}

func serveOptions(
	listen string, requestTimeout time.Duration, jwtSecret string, accessTokenTTL time.Duration,
) (bootstrap.AdminOptions, error) {
	var errs []string

	dsn, err := resolveDSN()
	if err != nil {
		errs = append(errs, strings.TrimPrefix(err.Error(), "admin: "))
	}
	if listen == "" {
		errs = append(errs, "--listen is required")
	}
	if requestTimeout <= 0 || requestTimeout > bootstrap.MaxAdminRequestTimeout {
		errs = append(errs, fmt.Sprintf(
			"--request-timeout must be above 0s and at most %s, got %s",
			bootstrap.MaxAdminRequestTimeout, requestTimeout))
	}

	secret := resolveJWTSecret(jwtSecret)
	switch {
	case secret == "":
		errs = append(errs, "no signing secret configured: pass --jwt-secret, or set GO_ROUTE_JWT_SECRET")
	case len(secret) < tokens.MinSecretBytes:
		errs = append(errs, fmt.Sprintf(
			"--jwt-secret must be at least %d bytes, got %d", tokens.MinSecretBytes, len(secret)))
	}
	if accessTokenTTL < tokens.MinAccessTTL || accessTokenTTL > tokens.MaxAccessTTL {
		errs = append(errs, fmt.Sprintf(
			"--access-token-ttl must be between %s and %s, got %s",
			tokens.MinAccessTTL, tokens.MaxAccessTTL, accessTokenTTL))
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return bootstrap.AdminOptions{}, fmt.Errorf("admin: invalid:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return bootstrap.AdminOptions{
		DSN:            dsn,
		Listen:         listen,
		RequestTimeout: requestTimeout,
		JWTSecret:      []byte(secret),
		AccessTokenTTL: accessTokenTTL,
	}, nil
}

func resolveJWTSecret(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("GO_ROUTE_JWT_SECRET")
}
