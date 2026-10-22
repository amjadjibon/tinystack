package app

import (
	"context"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/amjadjibon/tinystack/internal/console"
)

// newConsole listens on address and returns the server for the console's
// files and TinyStack's own endpoints.
func newConsole(
	ctx context.Context, address string, files fs.FS, api http.Handler,
) (*http.Server, net.Listener, error) {
	h, err := console.Handler(files, api, address)
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
