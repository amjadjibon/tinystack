package cmd

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
)

func newStartCommand() *cobra.Command {
	var configPath, host, initDir, authorization string
	var port, consolePort int
	var executor, python, node, dnsAddress string
	start := &cobra.Command{
		Use:   "start",
		Short: "Run the local AWS endpoint in the foreground",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}

			envReport, err := config.ApplyLocalStackEnv(&cfg, nil, nil)
			if err != nil {
				return err
			}

			for key, target := range map[string]*string{
				"TINYSTACK_LAMBDA_EXECUTOR":     &cfg.Lambda.Executor,
				"TINYSTACK_LAMBDA_LOCAL_PYTHON": &cfg.Lambda.Local.Python,
				"TINYSTACK_LAMBDA_LOCAL_NODE":   &cfg.Lambda.Local.Node,
			} {
				if value, present := os.LookupEnv(key); present {
					*target = value
				}
			}

			if cmd.Flags().Changed("lambda-executor") {
				cfg.Lambda.Executor = executor
			}

			if cmd.Flags().Changed("lambda-local-python") {
				cfg.Lambda.Local.Python = python
			}

			if cmd.Flags().Changed("lambda-local-node") {
				cfg.Lambda.Local.Node = node
			}

			if cmd.Flags().Changed("dns-address") {
				cfg.DNS.Enabled = true
				cfg.DNS.Address = dnsAddress
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

			if cmd.Flags().Changed("init-dir") {
				cfg.Init.Dir = initDir
			}

			if cmd.Flags().Changed("authorization") {
				cfg.Identity.Authorization = authorization
			}

			if guardErr := cfg.Validate(); guardErr != nil {
				return guardErr
			}

			var handlerOpts *slog.HandlerOptions
			if envReport.LogLevelSet {
				handlerOpts = &slog.HandlerOptions{Level: envReport.LogLevel}
			}

			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), handlerOpts))
			for _, warn := range envReport.Warnings {
				logger.Warn(warn)
			}

			return app.Run(cmd.Context(), cfg, logger)
		},
	}

	start.Flags().
		StringVar(&authorization, "authorization", config.AuthorizationOff, "IAM authorization: off, soft or enforce")
	start.Flags().StringVar(&configPath, "config", "", "YAML configuration path")
	start.Flags().StringVar(&dnsAddress, "dns-address", "127.0.0.1:53", "enable DNS on this UDP/TCP address")
	start.Flags().StringVar(&host, "host", "127.0.0.1", "listen host")
	start.Flags().IntVar(&port, "port", 4566, "listen port")
	start.Flags().IntVar(&consolePort, "console-port", 4567, "web console port (0 turns it off)")
	start.Flags().StringVar(
		&initDir, "init-dir", config.DefaultInitDir, "directory of <stage>.d init scripts (empty turns them off)",
	)
	start.Flags().StringVar(&executor, "lambda-executor", config.ExecutorAuto, "Lambda executor: auto, docker or local")
	start.Flags().StringVar(&python, "lambda-local-python", "python3", "local Lambda Python interpreter")
	start.Flags().StringVar(&node, "lambda-local-node", "node", "local Lambda Node interpreter")
	return start
}
