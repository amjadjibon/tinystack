//go:build !console

package console

import "net/http"

// mcpHandler is nil without the console build tag: headless builds have no MCP
// server.
func mcpHandler(http.Handler, string) http.Handler { return nil }
