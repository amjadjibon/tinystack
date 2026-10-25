package bedrockagentcorecontrol

import (
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Target is what an invocation of a runtime endpoint runs.
type Target struct {
	// Environment is the runtime's environment variables.
	Environment map[string]string
	RuntimeID   string
	RuntimeARN  string
	// Endpoint is the endpoint's name.
	Endpoint string
	// Image is the container image of the version the endpoint serves.
	Image string
	// Protocol is HTTP, MCP or A2A.
	Protocol string
	// AccessKey is the runtime creator's, which signs the SDK calls the container makes.
	AccessKey string
	// IdleTimeout and MaxLifetime bound a session's container.
	IdleTimeout time.Duration
	MaxLifetime time.Duration
	// Version is the version the endpoint serves.
	Version int
}

// FindTarget returns what the endpoint of runtime runtimeID named qualifier serves, for the data plane. An empty
// qualifier names the DEFAULT endpoint. An endpoint that serves no version yet is a validation failure.
func FindTarget(tx storage.ReadTx, scope request.Scope, runtimeID, qualifier string) (*Target, error) {
	r, err := readRuntime(tx, scope, runtimeID)
	if err != nil {
		return nil, err
	}

	if r.Deleting {
		return nil, notFound("AgentRuntime %s does not exist", runtimeID)
	}

	if qualifier == "" {
		qualifier = DefaultEndpoint
	}

	ep, err := readEndpoint(tx, scope, r.ID, qualifier)
	if err != nil {
		return nil, err
	}

	switch {
	case ep.Status == statusDeleting:
		return nil, notFound("Endpoint %s of AgentRuntime %s does not exist", qualifier, runtimeID)
	case ep.Live == 0:
		return nil, invalid(
			"Endpoint %s of AgentRuntime %s is %s and serves no version yet",
			qualifier,
			r.ID,
			ep.Status,
		)
	}

	v, err := readVersion(tx, scope, r.ID, ep.Live)
	if err != nil {
		return nil, err
	}

	c := &v.Config
	protocol := protocolHTTP
	if c.Protocol != nil {
		protocol = common.Value(c.Protocol.ServerProtocol)
	}

	return &Target{
		Environment: c.Environment, RuntimeID: r.ID, RuntimeARN: runtimeARN(scope, r.ID), Endpoint: ep.Name,
		Image: common.Value(c.Artifact.ContainerConfiguration.ContainerUri), Protocol: protocol, AccessKey: r.AccessKey,
		IdleTimeout: time.Duration(common.Value(c.Lifecycle.IdleRuntimeSessionTimeout)) * time.Second,
		MaxLifetime: time.Duration(common.Value(c.Lifecycle.MaxLifetime)) * time.Second, Version: v.Number,
	}, nil
}
