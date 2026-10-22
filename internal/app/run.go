package app

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/inithooks"
	"github.com/amjadjibon/tinystack/internal/localtls"
	"github.com/amjadjibon/tinystack/internal/services/dockercli"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/lambda/docker"
	"github.com/amjadjibon/tinystack/internal/services/lambda/local"
	"github.com/amjadjibon/tinystack/ui"
)

// shutdownScriptsTimeout bounds the shutdown stage, so a stuck script cannot
// keep TinyStack from exiting.
const shutdownScriptsTimeout = 30 * time.Second

// Run opens the configured storage, assembles TinyStack with the configured Lambda
// runtime, and serves it, plus the MQTT listeners and the web console, until
// ctx is cancelled or SIGINT or SIGTERM arrives. It returns nil after a clean
// shutdown. cfg must be valid.
//
// Signals are handled in serve, once everything is listening, so an interrupt
// during startup keeps its default effect and the shutdown path always has a
// running server to stop.
//
//nolint:gocritic // Configuration is passed by value like config.Load returns it.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	hooks := inithooks.New(cfg, logger)
	// Nothing listens yet, so boot scripts cannot reach TinyStack itself.
	hooks.Run(ctx, inithooks.Boot)
	store, closeStore, err := openStore(ctx, &cfg)
	if err != nil {
		return err
	}

	// Deferred first so it runs last, after Lambda's pollers stop writing.
	defer func() {
		if closeErr := closeStore(); closeErr != nil {
			logger.Error("close storage", "error", closeErr)
		}
	}()

	executor := cfg.Lambda.Executor
	if executor == config.ExecutorAuto {
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, probeErr := dockercli.Run(probe, nil, "info", "--format", "{{.ServerVersion}}")
		cancel()
		executor = config.ExecutorLocal
		if probeErr == nil {
			executor = config.ExecutorDocker
		}
	}

	var runtime lambda.Runtime
	if executor == config.ExecutorDocker {
		runtime = docker.New()
	} else {
		host := cfg.Server.Host
		if host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}

		runtime = local.New(local.Options{
			Python: cfg.Lambda.Local.Python, Node: cfg.Lambda.Local.Node,
			Endpoint: "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Server.Port)), Logger: logger,
		})
	}

	defer runtime.Close()
	logger.Info("Lambda executor selected", "executor", executor)
	if executor == config.ExecutorLocal {
		for _, interpreter := range []string{cfg.Lambda.Local.Python, cfg.Lambda.Local.Node} {
			if _, lookErr := exec.LookPath(interpreter); lookErr != nil {
				logger.Warn("local Lambda interpreter not found; its functions fail until it is installed, or run "+
					"with lambda.executor: docker and the Docker socket mounted", "interpreter", interpreter)
			}
		}
	}

	tinystack, err := build(ctx, cfg, store, runtime, logger, hooks)
	if err != nil {
		return err
	}

	// Deferred after the store's close so it runs first: nothing writes
	// to the store once Stop returns.
	defer tinystack.Stop()

	// The services exist but nothing listens yet.
	hooks.Run(ctx, inithooks.Start)
	return serve(ctx, cfg, tinystack, logger)
}

// serve listens for HTTP, MQTT, MQTTS and the console, and blocks until the
// servers stop or shutdown is requested.
//
//nolint:gocritic,contextcheck // See Run. Shutdown outlives ctx so in-flight requests can finish.
func serve(ctx context.Context, cfg config.Config, tinystack *App, logger *slog.Logger) error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(
		ctx,
		"tcp",
		net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
	)
	if err != nil {
		return err
	}

	defer func() { _ = listener.Close() }()
	server := gatewayServer(tinystack.Gateway)
	defer func() { _ = server.Close() }()
	var httpsServer *http.Server
	httpsDone := make(chan error, 1)
	if cfg.TLS.Enabled {
		tlsConfig, tlsErr := gatewayTLS(&cfg)
		if tlsErr != nil {
			return tlsErr
		}

		secure, tlsErr := listenConfig.Listen(ctx, "tcp", cfg.TLS.Address)
		if tlsErr != nil {
			return tlsErr
		}

		defer func() { _ = secure.Close() }()
		httpsServer = gatewayServer(tinystack.Gateway)
		defer func() { _ = httpsServer.Close() }()
		tinystack.StopOnShutdown(httpsServer)
		secure = tls.NewListener(secure, tlsConfig)
		go func() { httpsDone <- httpsServer.Serve(secure) }()
		logger.Info("https ready", "endpoint", "https://"+secure.Addr().String())
	}

	tinystack.StopOnShutdown(server)
	if cfg.IoT.MQTTPort != 0 {
		mqttListener, mqttErr := listenConfig.Listen(
			ctx,
			"tcp",
			net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.IoT.MQTTPort)),
		)
		if mqttErr != nil {
			_ = listener.Close()
			return mqttErr
		}

		tinystack.IoT.ListenMQTT(mqttListener)
		logger.Info("mqtt ready", "address", mqttListener.Addr().String())
	}

	if cfg.IoT.MQTTSPort != 0 {
		mqttsListener, mqttsErr := listenConfig.Listen(
			ctx,
			"tcp",
			net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.IoT.MQTTSPort)),
		)
		if mqttsErr != nil {
			_ = listener.Close()
			return mqttsErr
		}

		tinystack.IoT.ListenMQTTS(mqttsListener)
		logger.Info("mqtts ready", "address", mqttsListener.Addr().String())
	}

	// The console has its own listener, so its pages never reach the
	// AWS protocol classification.
	var consoleServer *http.Server
	consoleDone := make(chan error, 1)
	if files := ui.Files(); cfg.Server.ConsolePort != 0 && files == nil {
		logger.Info("this build has no web console; task build adds it")
	} else if cfg.Server.ConsolePort != 0 {
		var consoleListener net.Listener
		consoleServer, consoleListener, err = newConsole(
			ctx,
			net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.ConsolePort)),
			files,
			tinystack.Gateway.Internal(),
		)
		if err != nil {
			_ = listener.Close()
			return err
		}

		go func() { consoleDone <- consoleServer.Serve(consoleListener) }()
		logger.Info("console ready", "url", "http://"+consoleListener.Addr().String())
	}

	var imdsServer *http.Server
	imdsDone := make(chan error, 1)
	if cfg.Metadata.IMDS.Address != "" && tinystack.Metadata != nil {
		imdsListener, imdsErr := listenConfig.Listen(ctx, "tcp", cfg.Metadata.IMDS.Address)
		if imdsErr != nil {
			_ = listener.Close()
			return imdsErr
		}

		imdsServer = &http.Server{
			Handler:           tinystack.Metadata.IMDSHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}

		tinystack.StopOnShutdown(imdsServer)
		go func() { imdsDone <- imdsServer.Serve(imdsListener) }()
		logger.Info("imds ready", "address", imdsListener.Addr().String())
	}

	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
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
		strings.Join(tinystack.Services, ","),
	)
	// The listener is bound, so requests queue even before Serve takes them.
	tinystack.Init.Go(inithooks.Ready)
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case err = <-consoleDone:
		return err
	case err = <-httpsDone:
		return err
	case err = <-imdsDone:
		return err
	case <-signalCtx.Done():
	}

	// Ready scripts still running end, then the shutdown scripts run while
	// the listeners and the services still work.
	tinystack.Init.Stop()
	scripts, stopScripts := context.WithTimeout(context.WithoutCancel(ctx), shutdownScriptsTimeout)
	tinystack.Init.Run(scripts, inithooks.Shutdown)
	stopScripts()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if consoleServer != nil {
		_ = consoleServer.Shutdown(shutdown)
	}

	if httpsServer != nil {
		if tlsErr := httpsServer.Shutdown(shutdown); tlsErr != nil {
			return tlsErr
		}

		if tlsErr := <-httpsDone; !errors.Is(tlsErr, http.ErrServerClosed) {
			return tlsErr
		}
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
}

// gatewayServer serves the AWS gateway with the timeouts its HTTP and HTTPS
// listeners share.
func gatewayServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Synchronous Lambda invocations may run for the 15-minute
		// maximum function timeout.
		WriteTimeout: 16 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}
}

// gatewayTLS serves the configured certificate pair, or else leaves issued by
// the local CA in the data directory.
func gatewayTLS(cfg *config.Config) (*tls.Config, error) {
	if cfg.TLS.CertFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}

		return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, nil
	}

	dir, err := cfg.DataDir()
	if err != nil {
		return nil, err
	}

	authority, err := localtls.Load(dir)
	if err != nil {
		return nil, err
	}

	return authority.Config(cfg.TLS.Names)
}
