package sts

import (
	"context"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	"github.com/amjadjibon/tinystack/internal/common"
)

// sessionPolicies enforces AWS's plaintext limits. AWS's proprietary packed
// representation and PackedPolicySize percentage are not reproduced.
func (s *Service) sessionPolicies(ctx context.Context, account string, inline *string,
	descriptors []api.PolicyDescriptorType,
) ([]string, error) {
	if len(descriptors) > 10 {
		return nil, validation("PolicyArns must contain at most 10 managed policy ARNs")
	}

	arns := make([]string, 0, len(descriptors))
	size := utf8.RuneCountInString(common.Value(inline))
	for _, descriptor := range descriptors {
		if descriptor.Arn == nil || *descriptor.Arn == "" {
			return nil, validation("PolicyArns requires an ARN")
		}

		arns = append(arns, *descriptor.Arn)
		size += utf8.RuneCountInString(*descriptor.Arn)
	}

	if size > 2048 {
		return nil, validation("Session policy plaintext must not exceed 2048 characters")
	}

	if inline != nil && *inline == "" {
		return nil, awserror.New("MalformedPolicyDocumentException", "Empty session policy")
	}

	if err := s.roles.ValidateSessionPolicies(ctx, account, common.Value(inline), arns); err != nil {
		return nil, awserror.New("MalformedPolicyDocumentException", err.Error())
	}

	return arns, nil
}
