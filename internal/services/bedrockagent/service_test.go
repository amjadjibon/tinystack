package bedrockagent

import (
	"context"
	"strings"
	"testing"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestGeneratedInputConstraints(t *testing.T) {
	store := memory.New(nil, nil)
	service := New(store, bedrock.New(store))
	descriptor := api.Descriptor(service)
	for _, input := range []*api.CreateAgentRequest{
		{},
		{AgentName: new("invalid name"), AgentResourceRoleArn: new("arn:aws:iam::000000000000:role/role")},
		{AgentName: new("valid"), IdleSessionTTLInSeconds: new(int64(1))},
	} {
		if _, checkErr1 := descriptor.Operations["CreateAgent"].Invoke(
			context.Background(),
			input,
		); checkErr1 == nil ||
			!strings.Contains(checkErr1.Error(), "validation") {
			t.Errorf("expected generated validation: %v", checkErr1)
		}
	}
}
