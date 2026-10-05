package cmd

import (
	"context"
	"errors"
	"io/fs"
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
	acmapi "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	apigatewayapi "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	apigatewayv2api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	appsyncapi "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	autoscalingapi "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	cloudformationapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	cloudfrontapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	cloudwatchapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	cognitoidpapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	dynamodbstreamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	ecrapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	elbv2api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	firehoseapi "github.com/amjadjibon/tinystack/internal/aws/generated/firehose"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	kinesisapi "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	logsapi "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	route53api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	schedulerapi "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	secretsmanagerapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	sfnapi "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	ssmapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/console"
	"github.com/amjadjibon/tinystack/internal/delivery"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/acm"
	"github.com/amjadjibon/tinystack/internal/services/apigateway"
	"github.com/amjadjibon/tinystack/internal/services/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/services/appsync"
	"github.com/amjadjibon/tinystack/internal/services/autoscaling"
	"github.com/amjadjibon/tinystack/internal/services/cloudformation"
	"github.com/amjadjibon/tinystack/internal/services/cloudfront"
	"github.com/amjadjibon/tinystack/internal/services/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/ecr"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	ecsdocker "github.com/amjadjibon/tinystack/internal/services/ecs/docker"
	"github.com/amjadjibon/tinystack/internal/services/eks"
	"github.com/amjadjibon/tinystack/internal/services/eks/k3s"
	"github.com/amjadjibon/tinystack/internal/services/elbv2"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/firehose"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/lambda/docker"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/route53"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/scheduler"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/services/sfn"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/ssm"
	"github.com/amjadjibon/tinystack/internal/services/sts"
	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/sqlite"
	"github.com/amjadjibon/tinystack/ui"
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
			// Stop ends HTTP deliveries in flight.
			defer topics.Stop()
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
			tokens.UseAccounts(cfg.Identity.Accounts, cfg.Defaults.Account)
			identities := cognitoidp.New(store, functions, deliveries)
			registry := ecr.New(store)
			containers := ecs.New(store)
			if cfg.ECS.Containers == config.ContainersDocker {
				containers.UseRuntime(ecsdocker.New(), logGroups.Append, logger)
			}

			// Stop ends the task runner and removes task containers.
			defer containers.Stop()
			httpAPIs := apigatewayv2.New(store, functions, logGroups)
			restAPIs := apigateway.New(store, functions, logGroups, identities)
			graphQL := appsync.New(store, functions, identities)
			network := ec2.New(store)
			network.UseInstanceProfiles(roles.InstanceProfileOf)
			kube := eks.New(store, network)
			// Stop ends the cluster runner and removes k3s containers.
			defer kube.Stop()
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
			dns := route53.New(store)
			cdn := cloudfront.New(store)
			timers := scheduler.New(store, logger)
			defer timers.Stop()
			scaling := autoscaling.New(store, logger)
			defer scaling.Stop()
			balancers := elbv2.New(store, logger)
			stacks := cloudformation.New(store, logger)
			// Stop ends the stack runner, which writes to the store.
			defer stacks.Stop()
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
				ecsapi.Descriptor(containers),
				eksapi.Descriptor(kube),
				apigatewayv2api.Descriptor(httpAPIs),
				apigatewayapi.Descriptor(restAPIs),
				appsyncapi.Descriptor(graphQL),
				sfnapi.Descriptor(workflows),
				kinesisapi.Descriptor(streams),
				route53api.Descriptor(dns),
				cloudfrontapi.Descriptor(cdn),
				schedulerapi.Descriptor(timers),
				autoscalingapi.Descriptor(scaling),
				elbv2api.Descriptor(balancers),
				acmapi.Descriptor(acm.New(store)),
				firehoseapi.Descriptor(firehoses),
				cloudwatchapi.Descriptor(monitoring),
				cloudformationapi.Descriptor(stacks),
			}
			// Step Functions' Task states and Firehose's deliveries call the
			// other services in-process.
			caller := catalog.NewCaller(services...)
			workflows.UseCaller(caller)
			firehoses.UseCaller(caller)
			monitoring.UseCaller(caller)
			dns.UseCaller(caller)
			timers.UseCaller(caller)
			scaling.UseCaller(caller)
			balancers.UseCaller(caller)
			// CloudFormation manages stack resources with the other
			// services' operations.
			stacks.UseCaller(caller)
			// Subscription filters deliver to Lambda, Kinesis, and Firehose.
			logGroups.UseCaller(caller)
			// Table exports write to S3.
			tables.UseCaller(caller)
			// Asynchronous invocations send results to their destinations.
			functions.UseCaller(caller)
			handler, err := gateway.New(cfg, logger, services...)
			if err != nil {
				return err
			}

			handler.UseCredentials(tokens.Credentials)
			handler.UseDocuments(identities.Documents)
			handler.UseTokens(identities.Token)
			handler.UseRegistry(registry)
			handler.UseCORS(buckets.CORS)
			handler.UseAPIs(httpAPIs, restAPIs)
			handler.UseDomains(restAPIs.Domains())
			handler.UseLoadBalancers(balancers)
			handler.UseFunctionURLs(functions.URLs())
			handler.UseGraphQL(graphQL)
			if cfg.EKS.Clusters == config.ClustersK3s {
				// Cluster endpoints accept the EKS tokens the gateway's
				// credentials sign.
				clusters, k3sErr := k3s.New(handler.KubernetesAccount, logger)
				if k3sErr != nil {
					return k3sErr
				}

				kube.UseRuntime(clusters, logger)
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
			server.RegisterOnShutdown(topics.Stop)
			server.RegisterOnShutdown(tables.Stop)
			server.RegisterOnShutdown(functions.Stop)
			server.RegisterOnShutdown(rules.Stop)
			server.RegisterOnShutdown(secrets.Stop)
			server.RegisterOnShutdown(workflows.Stop)
			server.RegisterOnShutdown(firehoses.Stop)
			server.RegisterOnShutdown(monitoring.Stop)
			server.RegisterOnShutdown(timers.Stop)
			server.RegisterOnShutdown(scaling.Stop)
			server.RegisterOnShutdown(containers.Stop)
			server.RegisterOnShutdown(kube.Stop)
			server.RegisterOnShutdown(stacks.Stop)
			// The console has its own listener, so its pages never reach the
			// AWS protocol classification.
			var consoleServer *http.Server
			consoleDone := make(chan error, 1)
			if files := ui.Files(); cfg.Server.ConsolePort != 0 && files == nil {
				logger.Info("this build has no web console; task build adds it")
			} else if cfg.Server.ConsolePort != 0 {
				var consoleListener net.Listener
				consoleServer, consoleListener, err = newConsole(
					cmd.Context(),
					net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.ConsolePort)),
					files,
					handler.Internal(),
				)
				if err != nil {
					_ = listener.Close()
					return err
				}

				go func() { consoleDone <- consoleServer.Serve(consoleListener) }()
				logger.Info("console ready", "url", "http://"+consoleListener.Addr().String())
			}

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
			case err = <-consoleDone:
				return err
			case <-ctx.Done():
			}

			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if consoleServer != nil {
				_ = consoleServer.Shutdown(shutdown)
			}

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
	start.Flags().IntVar(&consolePort, "console-port", 4567, "web console port (0 turns it off)")
	return start
}

// openStore returns the store the storage mode selects, keeping persistent
// state in dir, and the function that releases it.
// newConsole listens on address and returns the server for the console's
// files and TinyStack's own endpoints.
func newConsole(
	ctx context.Context, address string, files fs.FS, api http.Handler,
) (*http.Server, net.Listener, error) {
	h, err := console.Handler(files, api)
	if err != nil {
		return nil, nil, err
	}

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, nil, err
	}

	server := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return server, listener, nil
}

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
