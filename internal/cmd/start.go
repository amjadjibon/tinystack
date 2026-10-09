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

	"github.com/amjadjibon/tinystack/internal/app"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/console"
	"github.com/amjadjibon/tinystack/internal/services/lambda/docker"
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

			tinystack, err := app.New(cmd.Context(), cfg, store, docker.New(), logger)
			if err != nil {
				return err
			}

			// Deferred after the store's close so it runs first: nothing writes
			// to the store once Stop returns.
			defer tinystack.Stop()

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
				Handler:           tinystack.Gateway,
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       30 * time.Second,
				// Synchronous Lambda invocations may run for the 15-minute
				// maximum function timeout.
				WriteTimeout: 16 * time.Minute,
				IdleTimeout:  60 * time.Second,
			}

			tinystack.StopOnShutdown(server)
			if cfg.IoT.MQTTPort != 0 {
				mqttListener, mqttErr := listenConfig.Listen(
					cmd.Context(),
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
					cmd.Context(),
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
					cmd.Context(),
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
				strings.Join(tinystack.Services, ","),
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
