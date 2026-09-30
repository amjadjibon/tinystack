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

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	apigatewayapi "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	apigatewayv2api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	cloudwatchapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	cognitoidpapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	dynamodbstreamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	ecrapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	firehoseapi "github.com/amjadjibon/tinystack/internal/aws/generated/firehose"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	kinesisapi "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	logsapi "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	secretsmanagerapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	sfnapi "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	ssmapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/apigateway"
	"github.com/amjadjibon/tinystack/internal/services/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/services/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/ecr"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	"github.com/amjadjibon/tinystack/internal/services/eks"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/firehose"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/lambda/docker"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/services/sfn"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/ssm"
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
			tables := dynamodb.New(store)
			// Stop ends the TTL sweeper, which writes to the store.
			defer tables.Stop()
			keys := kms.New(store)
			queues := sqs.New(store, keys)
			// Stop ends the message mover, which writes to the store.
			defer queues.Stop()
			logGroups := logs.New(store)
			functions := lambda.New(
				store,
				queues,
				tables.Streams(),
				logGroups,
				docker.New(),
				logger,
				cfg.Identity.SecretKeys,
				deliveries,
			)
			// Stop removes function containers; it must finish before exit.
			defer functions.Stop()
			streams := kinesis.New(store)
			functions.UseKinesis(streams)
			if guardErr := functions.Resume(cmd.Context()); guardErr != nil {
				return guardErr
			}

			topics := sns.New(store, queues, functions, deliveries)
			rules := eventbridge.New(store, queues, topics, functions, deliveries)
			// Stop ends the scheduler, which writes to the store.
			defer rules.Stop()
			buckets := s3.New(store, queues, topics, functions, rules, deliveries)
			secrets := secretsmanager.New(store, keys, functions, deliveries)
			// Stop ends the rotation scheduler and running rotations.
			defer secrets.Stop()
			functions.UseObjects(buckets.Object)
			roles := iam.New(store)
			tokens := sts.New(store, roles)
			identities := cognitoidp.New(store, deliveries)
			registry := ecr.New(store)
			httpAPIs := apigatewayv2.New(store, functions, logGroups)
			restAPIs := apigateway.New(store, functions, logGroups, identities)
			network := ec2.New(store)
			workflows, err := sfn.New(store, logGroups, logger)
			if err != nil {
				return err
			}

			// Stop ends running executions.
			defer workflows.Stop()
			firehoses := firehose.New(store, streams, deliveries, logger)
			// Stop ends the flusher, which writes to the store.
			defer firehoses.Stop()
			monitoring := cloudwatch.New(store, logger)
			defer monitoring.Stop()
			parameters := ssm.New(store, keys)
			// Systems Manager publishes the IDs of EC2's images.
			parameters.UsePublicParameters(ec2.PublicParameters)
			services := []catalog.Service{
				dynamodbapi.Descriptor(tables),
				dynamodbstreamsapi.Descriptor(tables.Streams()),
				sqsapi.Descriptor(queues),
				snsapi.Descriptor(topics),
				s3api.Descriptor(buckets),
				lambdaapi.Descriptor(functions),
				eventbridgeapi.Descriptor(rules),
				iamapi.Descriptor(roles),
				stsapi.Descriptor(tokens),
				ssmapi.Descriptor(parameters),
				secretsmanagerapi.Descriptor(secrets),
				logsapi.Descriptor(logGroups),
				kmsapi.Descriptor(keys),
				ec2api.Descriptor(network),
				cognitoidpapi.Descriptor(identities),
				ecrapi.Descriptor(registry),
				ecsapi.Descriptor(ecs.New(store)),
				eksapi.Descriptor(eks.New(store, network)),
				apigatewayv2api.Descriptor(httpAPIs),
				apigatewayapi.Descriptor(restAPIs),
				sfnapi.Descriptor(workflows),
				kinesisapi.Descriptor(streams),
				firehoseapi.Descriptor(firehoses),
				cloudwatchapi.Descriptor(monitoring),
			}
			// Step Functions' Task states and Firehose's deliveries call the
			// other services in-process.
			caller := catalog.NewCaller(services...)
			workflows.UseCaller(caller)
			firehoses.UseCaller(caller)
			monitoring.UseCaller(caller)
			handler, err := gateway.New(cfg, logger, services...)
			if err != nil {
				return err
			}

			handler.UseCredentials(tokens.Credentials)
			handler.UseDocuments(identities.Documents)
			handler.UseRegistry(registry)
			handler.UseAPIs(httpAPIs, restAPIs)
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
			server.RegisterOnShutdown(tables.Stop)
			server.RegisterOnShutdown(functions.Stop)
			server.RegisterOnShutdown(rules.Stop)
			server.RegisterOnShutdown(secrets.Stop)
			server.RegisterOnShutdown(workflows.Stop)
			server.RegisterOnShutdown(firehoses.Stop)
			server.RegisterOnShutdown(monitoring.Stop)
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
				"dynamodb,dynamodbstreams,eventbridge,iam,kms,lambda,logs,s3,secretsmanager,sns,sqs,ssm,sts",
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
