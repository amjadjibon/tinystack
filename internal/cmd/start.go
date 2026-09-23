package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func newStartCommand() *cobra.Command {
	var configPath, host string
	var port int
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

			if guardErr := cfg.Validate(); guardErr != nil {
				return guardErr
			}

			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			store := &memory.Store{}
			queues := sqs.New(store)
			handler, err := gateway.New(
				cfg,
				logger,
				dynamodbapi.Descriptor(dynamodb.New(store)),
				sqsapi.Descriptor(queues),
				snsapi.Descriptor(sns.New(store, queues)),
				s3api.Descriptor(s3.New(store)),
			)
			if err != nil {
				return err
			}

			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(
				cmd.Context(),
				"tcp",
				net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
			)
			if err != nil {
				return err
			}

			server := &http.Server{
				Handler:           handler,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       60 * time.Second,
			}

			server.RegisterOnShutdown(queues.Stop)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			logger.Info(
				"tinystack ready",
				"endpoint",
				"http://"+listener.Addr().String(),
				"storage",
				"memory",
				"services",
				"dynamodb,s3,sns,sqs",
			)
			select {
			case err = <-done:
				if errors.Is(err, http.ErrServerClosed) {
					return nil
				}

				return err
			case <-ctx.Done():
			}

			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if guardErr := server.Shutdown(shutdown); guardErr != nil {
				closeErr := server.Close()
				return errors.Join(guardErr, closeErr)
			}

			err = <-done
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		},
	}

	start.Flags().StringVar(&configPath, "config", "", "YAML configuration path")
	start.Flags().StringVar(&host, "host", "127.0.0.1", "listen host")
	start.Flags().IntVar(&port, "port", 4566, "listen port")
	return start
}
