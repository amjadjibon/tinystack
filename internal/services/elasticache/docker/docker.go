// Package docker provides the ElastiCache container runtime.
package docker

import "github.com/amjadjibon/tinystack/internal/services/valkeydocker"

// Runtime is the shared cache container runtime.
type Runtime = valkeydocker.Runtime

// New constructs the shared cache container runtime.
func New() *Runtime { return valkeydocker.New() }
