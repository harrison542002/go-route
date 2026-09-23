package main

import (
	"errors"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var dsnFlag string

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "go-route-admin",
		Short: "The internal admin API: tenants, API keys and quotas",
		Long: `go-route-admin serves the internal admin API and makes the credentials
that reach it: named tokens for machines, and accounts with a password
for people. It talks only to Postgres, so it needs a DSN and nothing
else - no config file, and no running proxy.

The proxy's YAML describes providers, targets, aliases and pricing, none
of which this service reads. Rather than point it at a file it would
ignore, everything it needs is a flag, with DATABASE_URL as the fallback
for the one setting a deployment already has in its environment.`,
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&dsnFlag, "dsn", "",
		"database DSN (defaults to $DATABASE_URL)")

	root.AddCommand(
		serveCmd(),
		createTokenCmd(), listTokensCmd(), revokeTokenCmd(),
		createUserCmd(), listUsersCmd(), disableUserCmd(), setPasswordCmd(),
	)
	return root
}

func resolveDSN() (string, error) {
	if dsnFlag != "" {
		return dsnFlag, nil
	}
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		return dsn, nil
	}
	return "", errors.New(
		"admin: no database configured: pass --dsn, or set DATABASE_URL")
}
