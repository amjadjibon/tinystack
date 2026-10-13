package sts

import (
	"context"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
)

func (s *Service) AuthorizationResources(
	_ context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	if operation != "AssumeRole" {
		return nil, nil
	}

	arn := catalog.InputString(input, "RoleArn")
	if arn == "" {
		return nil, nil
	}

	return []catalog.AuthorizationResource{{ARN: arn}}, nil
}
