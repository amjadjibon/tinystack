package bedrockagent

import api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"

func agentVersion(a *api.Agent) *api.AgentVersion {
	return &api.AgentVersion{
		Version:                     a.AgentVersion,
		AgentId:                     a.AgentId,
		AgentName:                   a.AgentName,
		AgentArn:                    a.AgentArn,
		Instruction:                 a.Instruction,
		AgentStatus:                 a.AgentStatus,
		FoundationModel:             a.FoundationModel,
		Description:                 a.Description,
		IdleSessionTTLInSeconds:     a.IdleSessionTTLInSeconds,
		AgentResourceRoleArn:        a.AgentResourceRoleArn,
		CustomerEncryptionKeyArn:    a.CustomerEncryptionKeyArn,
		CreatedAt:                   a.CreatedAt,
		UpdatedAt:                   a.UpdatedAt,
		FailureReasons:              a.FailureReasons,
		RecommendedActions:          a.RecommendedActions,
		PromptOverrideConfiguration: a.PromptOverrideConfiguration,
		GuardrailConfiguration:      a.GuardrailConfiguration,
		MemoryConfiguration:         a.MemoryConfiguration,
		AgentCollaboration:          a.AgentCollaboration,
	}
}
