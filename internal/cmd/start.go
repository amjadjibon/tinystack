package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/lambda/docker"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/sts"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
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
			store, closeStore, err := openStore(cmd.Context(), cfg.Storage.Mode, cfg.Storage.Path)
			if err != nil {
				return err
			}

			// Deferred first so it runs last, after Lambda's pollers stop writing.
			defer func() {
				if closeErr := closeStore(); closeErr != nil {
					logger.Error("close storage", "error", closeErr)
				}
			}()

			deliveries := delivery.New()
			queues := sqs.New(store)
			functions := lambda.New(store, queues, docker.New(), logger, cfg.Identity.SecretKeys, deliveries)
			// Stop removes function containers; it must finish before exit.
			defer functions.Stop()
			if guardErr := functions.Resume(cmd.Context()); guardErr != nil {
				return guardErr
			}

			topics := sns.New(store, queues, functions, deliveries)
			handler, err := gateway.New(
				cfg,
				logger,
				dynamodbapi.Descriptor(dynamodb.New(store)),
				sqsapi.Descriptor(queues),
				snsapi.Descriptor(topics),
				s3api.Descriptor(s3.New(store)),
				lambdaapi.Descriptor(functions),
				eventbridgeapi.Descriptor(eventbridge.New(store, queues, topics, functions, deliveries)),
				iamapi.Descriptor(iam.New(store)),
				stsapi.Descriptor(sts.New()),
			)
			if err != nil {
				return err
			}

			handler.Handle("deliveries", deliveries)

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
				// Synchronous Lambda invocations may run for the 15-minute
				// maximum function timeout.
				WriteTimeout: 16 * time.Minute,
				IdleTimeout:  60 * time.Second,
			}

			server.RegisterOnShutdown(queues.Stop)
			server.RegisterOnShutdown(functions.Stop)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			logger.Info(
				"tinystack ready",
				"endpoint",
				"http://"+listener.Addr().String(),
				"storage",
				cfg.Storage.Mode,
				"services",
				"dynamodb,eventbridge,iam,lambda,s3,sns,sqs,sts",
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

// openStore returns the store the storage mode selects, keeping persistent
// state in dir, and the function that releases it.
func openStore(ctx context.Context, mode, dir string) (storage.Store, func() error, error) {
	if mode == config.StorageMemory {
		return &memory.Store{}, func() error { return nil }, nil
	}

	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, err
		}

		dir = filepath.Join(home, dir[1:])
	}

	store, err := sqlite.Open(ctx, dir)
	if err != nil {
		return nil, nil, err
	}

	return store, store.Close, nil
}
