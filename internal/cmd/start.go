package cmd

import (
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
)

func newStartCommand() *cobra.Command {
	var configPath, host string
	var port, consolePort int
	start := &cobra.Command{
		Use:   "start",
		Short: "Run the local AWS endpoint in the foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			if cmd.Flags().Changed("host") {
				cfg.Server.Host = host
			}

			if cmd.Flags().Changed("port") {
				cfg.Server.Port = port
			}

			if cmd.Flags().Changed("console-port") {
				cfg.Server.ConsolePort = consolePort
			}

			if guardErr := cfg.Validate(); guardErr != nil {
				return guardErr
			}

			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			return app.Run(cmd.Context(), cfg, logger)
		},
	}

	start.Flags().StringVar(&configPath, "config", "", "YAML configuration path")
	start.Flags().StringVar(&host, "host", "127.0.0.1", "listen host")
	start.Flags().IntVar(&port, "port", 4566, "listen port")
	start.Flags().IntVar(&consolePort, "console-port", 4567, "web console port (0 turns it off)")
	return start
}
