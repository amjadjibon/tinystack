package ecs

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// ContainerMetadata holds metadata for an ECS task container.
type ContainerMetadata struct {
	Scope         request.Scope
	Cluster       string
	TaskARN       string
	TaskID        string
	Family        string
	Revision      string
	ContainerName string
	DockerID      string
	DockerName    string
	Image         string
	ImageID       string
	TaskRoleARN   string
	CreatedAt     time.Time
	StartedAt     time.Time
	CPU           *int64
	Memory        *int64
	Ports         []MetadataPortMapping
	Labels        map[string]string
	Networks      []MetadataNetwork
}

type MetadataPortMapping struct {
	BindIP        string `json:"BindIP"`
	Protocol      string `json:"Protocol"`
	ContainerPort int64  `json:"ContainerPort"`
	HostPort      int64  `json:"HostPort"`
}

type MetadataNetwork struct {
	NetworkMode   string   `json:"NetworkMode"`
	IPv4Addresses []string `json:"IPv4Addresses,omitempty"`
}

// ContainerMetadataByToken looks up a running task's container metadata.
func (s *Service) ContainerMetadataByToken(ctx context.Context, token string) (*ContainerMetadata, bool, error) {
	var result *ContainerMetadata
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		key, found, err := storage.Read[string](tx, "aws/ecs/metadata/"+token)
		if err != nil || !found {
			return err
		}

		t, found, err := storage.Read[task](tx, *key)
		if err != nil || !found || t.Last != running {
			return err
		}

		rec, found := t.Metadata[token]
		if found {
			result = &rec
		}

		return nil
	})

	return result, result != nil, err
}
