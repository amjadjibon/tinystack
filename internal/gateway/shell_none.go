//go:build !console

package gateway

// registerShell does nothing: the CloudShell is part of the console, so only
// binaries built with the console tag serve it.
func (s *Server) registerShell() {}
