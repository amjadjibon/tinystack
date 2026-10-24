package shield

import (
	"context"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func phone(contacts []api.EmergencyContact) bool {
	for _, c := range contacts {
		if common.Value(c.PhoneNumber) != "" {
			return true
		}
	}

	return false
}

func (s *Service) AssociateProactiveEngagementDetails(
	ctx context.Context,
	in *api.AssociateProactiveEngagementDetailsRequest,
) (*api.AssociateProactiveEngagementDetailsResponse, error) {
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		if !phone(in.EmergencyContactList) {
			return invalid("Proactive engagement requires at least one emergency phone number")
		}

		if r.Initialized {
			return noChange()
		}

		r.Contacts = in.EmergencyContactList
		r.Initialized = true
		r.Subscription.ProactiveEngagementStatus = new("ENABLED")
		return nil
	})
	return &api.AssociateProactiveEngagementDetailsResponse{}, err
}

func (s *Service) DescribeEmergencyContactSettings(
	ctx context.Context,
	_ *api.DescribeEmergencyContactSettingsRequest,
) (*api.DescribeEmergencyContactSettingsResponse, error) {
	out := &api.DescribeEmergencyContactSettingsResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		out.EmergencyContactList = r.Contacts
		return nil
	})
	return out, err
}

func (s *Service) UpdateEmergencyContactSettings(
	ctx context.Context,
	in *api.UpdateEmergencyContactSettingsRequest,
) (*api.UpdateEmergencyContactSettingsResponse, error) {
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		if common.Value(r.Subscription.ProactiveEngagementStatus) == "ENABLED" && !phone(in.EmergencyContactList) {
			return invalid("Enabled proactive engagement requires a phone number")
		}

		r.Contacts = in.EmergencyContactList
		return nil
	})
	return &api.UpdateEmergencyContactSettingsResponse{}, err
}

func (s *Service) proactive(ctx context.Context, enable bool) error {
	return s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		status := "DISABLED"
		if enable {
			status = "ENABLED"
		}

		if !r.Initialized || common.Value(r.Subscription.ProactiveEngagementStatus) == status {
			return noChange()
		}

		if enable && !phone(r.Contacts) {
			return invalid("Proactive engagement requires a phone number")
		}

		r.Subscription.ProactiveEngagementStatus = new(status)
		return nil
	})
}

func (s *Service) EnableProactiveEngagement(
	ctx context.Context,
	_ *api.EnableProactiveEngagementRequest,
) (*api.EnableProactiveEngagementResponse, error) {
	return &api.EnableProactiveEngagementResponse{}, s.proactive(ctx, true)
}

func (s *Service) DisableProactiveEngagement(
	ctx context.Context,
	_ *api.DisableProactiveEngagementRequest,
) (*api.DisableProactiveEngagementResponse, error) {
	return &api.DisableProactiveEngagementResponse{}, s.proactive(ctx, false)
}

func (s *Service) AssociateDRTRole(
	ctx context.Context,
	in *api.AssociateDRTRoleRequest,
) (*api.AssociateDRTRoleResponse, error) {
	if _, err := s.reference(ctx, common.Value(in.RoleArn), "role"); err != nil {
		return nil, err
	}

	err := s.transaction(
		ctx,
		func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error { r.Role = in.RoleArn; return nil },
	)
	return &api.AssociateDRTRoleResponse{}, err
}

func (s *Service) DisassociateDRTRole(
	ctx context.Context,
	_ *api.DisassociateDRTRoleRequest,
) (*api.DisassociateDRTRoleResponse, error) {
	err := s.transaction(
		ctx,
		func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error { r.Role = nil; return nil },
	)
	return &api.DisassociateDRTRoleResponse{}, err
}

func (s *Service) bucket(ctx context.Context, name string, add bool) error {
	if add {
		if s.caller == nil {
			return missing()
		}

		if _, err := s.caller.Call(ctx, "s3", "HeadBucket", map[string]any{"Bucket": name}); err != nil {
			return missing()
		}
	}

	return s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		if add {
			if r.Role == nil {
				return awserror.New("InvalidOperationException", "Associate a DRT role before adding log buckets")
			}

			if slices.Contains(r.Buckets, name) {
				return nil
			}

			if len(r.Buckets) >= 10 {
				return awserror.New("LimitsExceededException", "At most ten DRT log buckets are supported")
			}

			r.Buckets = append(r.Buckets, name)
		} else {
			r.Buckets = slices.DeleteFunc(r.Buckets, func(v string) bool { return v == name })
		}

		return nil
	})
}

func (s *Service) AssociateDRTLogBucket(
	ctx context.Context,
	in *api.AssociateDRTLogBucketRequest,
) (*api.AssociateDRTLogBucketResponse, error) {
	return &api.AssociateDRTLogBucketResponse{}, s.bucket(ctx, common.Value(in.LogBucket), true)
}

func (s *Service) DisassociateDRTLogBucket(
	ctx context.Context,
	in *api.DisassociateDRTLogBucketRequest,
) (*api.DisassociateDRTLogBucketResponse, error) {
	return &api.DisassociateDRTLogBucketResponse{}, s.bucket(ctx, common.Value(in.LogBucket), false)
}

func (s *Service) DescribeDRTAccess(
	ctx context.Context,
	_ *api.DescribeDRTAccessRequest,
) (*api.DescribeDRTAccessResponse, error) {
	out := &api.DescribeDRTAccessResponse{}
	err := s.transaction(ctx, func(_ storage.WriteTx, _ request.Scope, r *subscriptionRecord) error {
		out.RoleArn = r.Role
		out.LogBucketList = r.Buckets
		return nil
	})
	return out, err
}
