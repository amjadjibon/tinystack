//go:build console

package app

import (
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/services/sts"
)

func configureShell(handler *gateway.Server, tokens *sts.Service) {
	handler.UseShellIssuer(tokens.ConsoleSession)
}
