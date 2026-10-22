package apigatewaymanagementapi

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

// Resources is empty: live connections belong to their WebSocket API.
func (s *Service) Resources(context.Context) ([]catalog.Resource, error) { return nil, nil }
