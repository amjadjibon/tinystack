package autoscaling

import (
	"context"
	"encoding/base64"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const maxUserData = 21847

// CreateLaunchConfiguration stores a launch configuration of an image EC2
// has; configurations never change.
func (s *Service) CreateLaunchConfiguration(
	ctx context.Context, in *api.CreateLaunchConfigurationType,
) (*api.CreateLaunchConfigurationOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.LaunchConfigurationName)
	if checkErr := checkName("LaunchConfigurationName", name); checkErr != nil {
		return nil, checkErr
	}

	if in.ImageId == nil || in.InstanceType == nil {
		return nil, validation("ImageId and InstanceType are required.")
	}

	if t := value(in.PlacementTenancy); t != "" && t != "default" && t != "dedicated" {
		return nil, validation("PlacementTenancy must be default or dedicated.")
	}

	if u := value(in.UserData); u != "" {
		decoded, decodeErr := base64.StdEncoding.DecodeString(u)
		if decodeErr != nil || len(decoded) > maxUserData {
			return nil, validation("UserData must be base64 encoded and at most 16 KB.")
		}
	}

	if _, err = s.call(ctx, m.Scope, "ec2", "DescribeImages",
		map[string]any{"ImageIds": []any{*in.ImageId}}); err != nil {
		return nil, validation("The image id '[%s]' does not exist", *in.ImageId)
	}

	lc := &api.LaunchConfiguration{
		LaunchConfigurationName: in.LaunchConfigurationName,
		LaunchConfigurationARN: new(arn(m.Scope, "launchConfiguration:"+newID()+":launchConfigurationName/"+
			name)),
		ImageId: in.ImageId, KeyName: in.KeyName, SecurityGroups: in.SecurityGroups, UserData: in.UserData,
		InstanceType: in.InstanceType, InstanceMonitoring: in.InstanceMonitoring,
		IamInstanceProfile: in.IamInstanceProfile, CreatedTime: new(s.now().UTC()), EbsOptimized: in.EbsOptimized,
		AssociatePublicIpAddress: in.AssociatePublicIpAddress, PlacementTenancy: in.PlacementTenancy,
	}
	if lc.SecurityGroups == nil {
		lc.SecurityGroups = []string{}
	}

	if lc.InstanceMonitoring == nil {
		lc.InstanceMonitoring = &api.InstanceMonitoring{Enabled: new(true)}
	}

	if lc.EbsOptimized == nil {
		lc.EbsOptimized = new(false)
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(m.Scope, kindConfiguration, name)); found {
			return awserror.New("AlreadyExistsFault", "Launch Configuration by this name already exists - A launch "+
				"configuration already exists with the name "+name)
		}

		return write(tx, key(m.Scope, kindConfiguration, name), lc)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateLaunchConfigurationOutput{}, nil
}

func (s *Service) DescribeLaunchConfigurations(
	ctx context.Context, in *api.LaunchConfigurationNamesType,
) (*api.LaunchConfigurationsType, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var all []*api.LaunchConfiguration
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(m.Scope, kindConfiguration, "")) {
			lc, _, readErr := read[api.LaunchConfiguration](tx, k)
			if readErr != nil {
				return readErr
			}

			if len(in.LaunchConfigurationNames) == 0 ||
				slices.Contains(in.LaunchConfigurationNames, value(lc.LaunchConfigurationName)) {
				all = append(all, lc)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := window(
		all,
		func(lc *api.LaunchConfiguration) string { return value(lc.LaunchConfigurationName) },
		in.NextToken,
		in.MaxRecords,
	)
	if err != nil {
		return nil, err
	}

	out := &api.LaunchConfigurationsType{LaunchConfigurations: []api.LaunchConfiguration{}, NextToken: next}
	for _, lc := range page {
		out.LaunchConfigurations = append(out.LaunchConfigurations, *lc)
	}

	return out, nil
}

// DeleteLaunchConfiguration deletes a launch configuration no group uses.
func (s *Service) DeleteLaunchConfiguration(
	ctx context.Context, in *api.LaunchConfigurationNameType,
) (*api.DeleteLaunchConfigurationOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	name := value(in.LaunchConfigurationName)
	return &api.DeleteLaunchConfigurationOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(m.Scope, kindConfiguration, name)); !found {
			return validation("Launch configuration name not found - Launch configuration %s not found", name)
		}

		for _, k := range tx.Keys(key(m.Scope, kindGroup, "")) {
			g, _, err := read[group](tx, k)
			if err != nil {
				return err
			}

			if value(g.Group.LaunchConfigurationName) == name {
				return awserror.New("ResourceInUseFault", "Cannot delete launch configuration "+name+
					" because it is attached to AutoScalingGroup "+g.name())
			}
		}

		tx.Delete(key(m.Scope, kindConfiguration, name))
		return nil
	})
}
