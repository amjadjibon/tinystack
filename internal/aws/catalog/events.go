package catalog

import (
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
)

// ManagementEvent is a completed external AWS request. It deliberately carries
// neither request/response bodies nor authorization headers.
type ManagementEvent struct {
	Time                                                time.Time
	Metadata                                            request.Metadata
	SourceIPAddress, UserAgent, ErrorCode, ErrorMessage string
	Resources                                           []EventResource
}

// EventResource is an explicitly selected non-sensitive resource identifier.
type EventResource struct{ Type, Name string }
