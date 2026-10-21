package emr

import (
	"context"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// PutAutoScalingPolicy stores the auto-scaling policy for an instance group.
// In TinyStack, scaling policies are records only and scaling never actually runs.
func (s *Service) PutAutoScalingPolicy(
	ctx context.Context,
	in *api.PutAutoScalingPolicyInput,
) (*api.PutAutoScalingPolicyOutput, error) {
	if in.AutoScalingPolicy == nil || in.AutoScalingPolicy.Constraints == nil {
		return nil, invalid("AutoScalingPolicy and Constraints are required.")
	}

	constraints := in.AutoScalingPolicy.Constraints
	if constraints.MinCapacity == nil || constraints.MaxCapacity == nil {
		return nil, invalid("MinCapacity and MaxCapacity are required.")
	}

	if *constraints.MinCapacity < 0 || *constraints.MaxCapacity < 0 {
		return nil, invalid("Capacity values must be non-negative.")
	}

	if *constraints.MinCapacity > *constraints.MaxCapacity {
		return nil, invalid("MinCapacity must be less than or equal to MaxCapacity.")
	}

	if len(in.AutoScalingPolicy.Rules) == 0 {
		return nil, invalid("At least one scaling rule is required.")
	}

	for _, rule := range in.AutoScalingPolicy.Rules {
		if rule.Name == nil || strings.TrimSpace(*rule.Name) == "" {
			return nil, invalid("Rule Name is required.")
		}

		if rule.Action == nil || rule.Action.SimpleScalingPolicyConfiguration == nil {
			return nil, invalid("SimpleScalingPolicyConfiguration is required.")
		}

		cfg := rule.Action.SimpleScalingPolicyConfiguration
		if cfg.AdjustmentType == nil || *cfg.AdjustmentType == "" {
			return nil, invalid("AdjustmentType is required.")
		}

		if cfg.ScalingAdjustment == nil {
			return nil, invalid("ScalingAdjustment is required.")
		}

		if rule.Trigger == nil || rule.Trigger.CloudWatchAlarmDefinition == nil {
			return nil, invalid("CloudWatchAlarmDefinition is required.")
		}

		alarm := rule.Trigger.CloudWatchAlarmDefinition
		if alarm.ComparisonOperator == nil || *alarm.ComparisonOperator == "" {
			return nil, invalid("ComparisonOperator is required.")
		}

		if alarm.MetricName == nil || strings.TrimSpace(*alarm.MetricName) == "" {
			return nil, invalid("MetricName is required.")
		}

		if alarm.Period == nil {
			return nil, invalid("Period is required.")
		}

		if alarm.Threshold == nil {
			return nil, invalid("Threshold is required.")
		}
	}

	desc := &api.AutoScalingPolicyDescription{
		Status: &api.AutoScalingPolicyStatus{
			State: new("ATTACHED"),
		},
		Constraints: constraints,
		Rules:       in.AutoScalingPolicy.Rules,
	}

	var arn *string
	r, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		if len(r.Fleets) > 0 ||
			(r.Cluster.InstanceCollectionType != nil && *r.Cluster.InstanceCollectionType == "INSTANCE_FLEET") {
			return invalid("Auto scaling policies are not supported with instance fleets.")
		}

		target := common.Value(in.InstanceGroupId)
		for i := range r.Groups {
			if common.Value(r.Groups[i].Id) == target {
				r.Groups[i].AutoScalingPolicy = desc
				arn = r.Cluster.ClusterArn
				return nil
			}
		}

		return invalid("Instance group does not exist.")
	})
	if err != nil {
		return nil, err
	}

	return &api.PutAutoScalingPolicyOutput{
		ClusterId:         r.Cluster.Id,
		InstanceGroupId:   in.InstanceGroupId,
		AutoScalingPolicy: desc,
		ClusterArn:        arn,
	}, nil
}

// RemoveAutoScalingPolicy removes the auto-scaling policy from an instance group.
// In TinyStack, scaling policies are records only and scaling never actually runs.
func (s *Service) RemoveAutoScalingPolicy(
	ctx context.Context,
	in *api.RemoveAutoScalingPolicyInput,
) (*api.RemoveAutoScalingPolicyOutput, error) {
	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		if len(r.Fleets) > 0 ||
			(r.Cluster.InstanceCollectionType != nil && *r.Cluster.InstanceCollectionType == "INSTANCE_FLEET") {
			return invalid("Auto scaling policies are not supported with instance fleets.")
		}

		target := common.Value(in.InstanceGroupId)
		for i := range r.Groups {
			if common.Value(r.Groups[i].Id) == target {
				r.Groups[i].AutoScalingPolicy = nil
				return nil
			}
		}

		return invalid("Instance group does not exist.")
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveAutoScalingPolicyOutput{}, nil
}

// PutManagedScalingPolicy stores the managed scaling policy on the cluster record.
// In TinyStack, scaling policies are records only and scaling never actually runs.
func (s *Service) PutManagedScalingPolicy(
	ctx context.Context,
	in *api.PutManagedScalingPolicyInput,
) (*api.PutManagedScalingPolicyOutput, error) {
	if in.ManagedScalingPolicy == nil || in.ManagedScalingPolicy.ComputeLimits == nil {
		return nil, invalid("ManagedScalingPolicy and ComputeLimits are required.")
	}

	limits := in.ManagedScalingPolicy.ComputeLimits
	if limits.UnitType == nil {
		return nil, invalid("UnitType is required.")
	}

	unit := *limits.UnitType
	if unit != "Instances" && unit != "InstanceFleetUnits" && unit != "VCPU" {
		return nil, invalid("Invalid UnitType: must be Instances, InstanceFleetUnits, or VCPU.")
	}

	if limits.MinimumCapacityUnits == nil || limits.MaximumCapacityUnits == nil {
		return nil, invalid("MinimumCapacityUnits and MaximumCapacityUnits are required.")
	}

	if *limits.MinimumCapacityUnits < 0 || *limits.MaximumCapacityUnits < 0 {
		return nil, invalid("Capacity units must be non-negative.")
	}

	if *limits.MinimumCapacityUnits > *limits.MaximumCapacityUnits {
		return nil, invalid("MinimumCapacityUnits must be less than or equal to MaximumCapacityUnits.")
	}

	if limits.MaximumOnDemandCapacityUnits != nil {
		if *limits.MaximumOnDemandCapacityUnits < 0 ||
			*limits.MaximumOnDemandCapacityUnits > *limits.MaximumCapacityUnits {
			return nil, invalid("MaximumOnDemandCapacityUnits must be between 0 and MaximumCapacityUnits.")
		}
	}

	if limits.MaximumCoreCapacityUnits != nil {
		if *limits.MaximumCoreCapacityUnits < 0 ||
			*limits.MaximumCoreCapacityUnits > *limits.MaximumCapacityUnits {
			return nil, invalid("MaximumCoreCapacityUnits must be between 0 and MaximumCapacityUnits.")
		}
	}

	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		if err := mutable(r); err != nil {
			return err
		}

		r.ManagedScalingPolicy = in.ManagedScalingPolicy
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.PutManagedScalingPolicyOutput{}, nil
}

// GetManagedScalingPolicy returns the managed scaling policy stored on the cluster record.
// In TinyStack, scaling policies are records only and scaling never actually runs.
func (s *Service) GetManagedScalingPolicy(
	ctx context.Context,
	in *api.GetManagedScalingPolicyInput,
) (*api.GetManagedScalingPolicyOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var policy *api.ManagedScalingPolicy
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, in.ClusterId)
		if e != nil {
			return e
		}

		policy = r.ManagedScalingPolicy
		return nil
	})
	if err != nil {
		return nil, err
	}

	if policy == nil {
		return &api.GetManagedScalingPolicyOutput{}, nil
	}

	return &api.GetManagedScalingPolicyOutput{ManagedScalingPolicy: policy}, nil
}

// RemoveManagedScalingPolicy removes the managed scaling policy from the cluster record.
// In TinyStack, scaling policies are records only and scaling never actually runs.
func (s *Service) RemoveManagedScalingPolicy(
	ctx context.Context,
	in *api.RemoveManagedScalingPolicyInput,
) (*api.RemoveManagedScalingPolicyOutput, error) {
	_, err := s.change(ctx, in.ClusterId, func(r *record) error {
		r.ManagedScalingPolicy = nil
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.RemoveManagedScalingPolicyOutput{}, nil
}
