package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/harrison542002/go-route/internal/config"
	"github.com/harrison542002/go-route/internal/ports"
)

var (
	cfgPath string
	dsnFlag string
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "go-route",
		Short:        "An OpenAI-compatible LLM proxy with cost attribution",
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&cfgPath, "config",
		"configs/go-route.yaml", "path to the config file")

	root.AddCommand(serveCmd(), explainCmd(), reportCmd())
	return root
}

func loadConfig() (*config.Config, error) {
	return config.Load(cfgPath)
}

func storeDSN(cfg *config.Config) (string, error) {
	if dsnFlag != "" {
		return dsnFlag, nil
	}
	if cfg.Sink.Type != string(ports.POSTGRES) || cfg.Sink.DSN == "" {
		return "", fmt.Errorf(
			"no decision store configured: set sink.type to postgres in %s, or pass --dsn",
			cfgPath)
	}
	return cfg.Sink.DSN, nil
}
