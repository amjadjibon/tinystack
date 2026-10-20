package kinesisanalyticsv2

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// UpdateApplication updates configuration using an optimistic application version check.
func (s *Service) UpdateApplication(
	ctx context.Context,
	in *api.UpdateApplicationRequest,
) (*api.UpdateApplicationResponse, error) {
	// Resolve S3 before entering the write transaction. The version comparison below guards the read/modify/write.
	current, err := s.DescribeApplication(ctx, &api.DescribeApplicationRequest{ApplicationName: in.ApplicationName})
	if err != nil {
		return nil, err
	}

	if in.CurrentApplicationVersionId == nil ||
		common.Value(in.CurrentApplicationVersionId) != common.Value(current.ApplicationDetail.ApplicationVersionId) {
		return nil, checkVersion(&record{Detail: current.ApplicationDetail}, in.CurrentApplicationVersionId)
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var code *api.ApplicationCodeConfiguration
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, e := read(tx, scope, in.ApplicationName)
		if e != nil {
			return e
		}

		code = r.Code
		return nil
	})
	if err != nil {
		return nil, err
	}

	var cd *api.ApplicationCodeConfigurationDescription
	if u := in.ApplicationConfigurationUpdate; u != nil && u.ApplicationCodeConfigurationUpdate != nil {
		if content := u.ApplicationCodeConfigurationUpdate.CodeContentUpdate; content != nil {
			sources := 0
			if content.TextContentUpdate != nil {
				sources++
			}

			if content.ZipFileContentUpdate != nil {
				sources++
			}

			if content.S3ContentLocationUpdate != nil {
				sources++
			}

			if sources != 1 {
				return nil, invalid("Specify exactly one code content update source.")
			}
		}

		code = updateCode(code, u.ApplicationCodeConfigurationUpdate)
		cd, err = s.codeDescription(ctx, code)
		if err != nil {
			return nil, err
		}
	}

	r, err := s.change(
		ctx,
		in.ApplicationName,
		in.CurrentApplicationVersionId,
		func(tx storage.WriteTx, scope request.Scope, r *record) error {
			if *r.Detail.ApplicationStatus != ready && *r.Detail.ApplicationStatus != running {
				return inUse()
			}

			if in.ServiceExecutionRoleUpdate != nil {
				if e := checkRole(in.ServiceExecutionRoleUpdate); e != nil {
					return e
				}

				r.Detail.ServiceExecutionRole = in.ServiceExecutionRoleUpdate
			}

			if in.RuntimeEnvironmentUpdate != nil {
				if e := checkRuntime(in.RuntimeEnvironmentUpdate); e != nil {
					return e
				}

				r.Detail.RuntimeEnvironment = in.RuntimeEnvironmentUpdate
			}

			if e := s.updateConfig(tx, scope, r, in.ApplicationConfigurationUpdate); e != nil {
				return e
			}

			if cd != nil {
				r.Code = code
				r.Detail.ApplicationConfigurationDescription.ApplicationCodeConfigurationDescription = cd
			}

			if in.RunConfigurationUpdate != nil {
				if e := checkRun(tx, scope, r, (*api.RunConfiguration)(in.RunConfigurationUpdate)); e != nil {
					return e
				}
			}

			for _, u := range in.CloudWatchLoggingOptionUpdates {
				found := false
				for i, o := range r.Detail.CloudWatchLoggingOptionDescriptions {
					if common.Value(o.CloudWatchLoggingOptionId) == common.Value(u.CloudWatchLoggingOptionId) {
						if e := s.logs.CheckStreamARN(tx, scope, common.Value(u.LogStreamARNUpdate)); e != nil {
							return invalid("Invalid log stream: %s", e)
						}

						r.Detail.CloudWatchLoggingOptionDescriptions[i].LogStreamARN = u.LogStreamARNUpdate
						found = true
					}
				}

				if !found {
					return missing()
				}
			}

			revise(r)
			transition(r, "UPDATING", *r.Detail.ApplicationStatus)
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	return &api.UpdateApplicationResponse{ApplicationDetail: r.Detail}, nil
}

func updateCode(
	c *api.ApplicationCodeConfiguration,
	u *api.ApplicationCodeConfigurationUpdate,
) *api.ApplicationCodeConfiguration {
	c = common.Clone(c)
	if c == nil {
		c = &api.ApplicationCodeConfiguration{}
	}

	if u.CodeContentTypeUpdate != nil {
		c.CodeContentType = u.CodeContentTypeUpdate
	}

	if v := u.CodeContentUpdate; v != nil {
		if v.TextContentUpdate != nil || v.ZipFileContentUpdate != nil {
			c.CodeContent = &api.CodeContent{TextContent: v.TextContentUpdate, ZipFileContent: v.ZipFileContentUpdate}
		}

		if loc := v.S3ContentLocationUpdate; loc != nil {
			var previous *api.S3ContentLocation
			if c.CodeContent != nil {
				previous = c.CodeContent.S3ContentLocation
			}

			if previous == nil {
				previous = &api.S3ContentLocation{}
			}

			if loc.BucketARNUpdate != nil {
				previous.BucketARN = loc.BucketARNUpdate
			}

			if loc.FileKeyUpdate != nil {
				previous.FileKey = loc.FileKeyUpdate
			}

			if loc.ObjectVersionUpdate != nil {
				previous.ObjectVersion = loc.ObjectVersionUpdate
			}

			c.CodeContent = &api.CodeContent{S3ContentLocation: previous}
		}
	}

	return c
}

func (s *Service) updateConfig(
	tx storage.ReadTx,
	scope request.Scope,
	r *record,
	u *api.ApplicationConfigurationUpdate,
) error {
	if u == nil {
		return nil
	}

	d := r.Detail.ApplicationConfigurationDescription
	if u.EnvironmentPropertyUpdates != nil {
		d.EnvironmentPropertyDescriptions = &api.EnvironmentPropertyDescriptions{
			PropertyGroupDescriptions: u.EnvironmentPropertyUpdates.PropertyGroups,
		}
	}

	if u.ApplicationSnapshotConfigurationUpdate != nil {
		if u.ApplicationSnapshotConfigurationUpdate.SnapshotsEnabledUpdate == nil {
			return invalid("SnapshotsEnabledUpdate is required.")
		}

		snapshot := u.ApplicationSnapshotConfigurationUpdate
		d.ApplicationSnapshotConfigurationDescription.SnapshotsEnabled = snapshot.SnapshotsEnabledUpdate
	}

	if f := u.FlinkApplicationConfigurationUpdate; f != nil {
		if u := f.CheckpointConfigurationUpdate; u != nil {
			v := d.FlinkApplicationConfigurationDescription.CheckpointConfigurationDescription
			if u.ConfigurationTypeUpdate != nil {
				v.ConfigurationType = u.ConfigurationTypeUpdate
			}

			if u.CheckpointingEnabledUpdate != nil {
				v.CheckpointingEnabled = u.CheckpointingEnabledUpdate
			}

			if u.CheckpointIntervalUpdate != nil {
				v.CheckpointInterval = u.CheckpointIntervalUpdate
			}

			if u.MinPauseBetweenCheckpointsUpdate != nil {
				v.MinPauseBetweenCheckpoints = u.MinPauseBetweenCheckpointsUpdate
			}
		}

		if u := f.MonitoringConfigurationUpdate; u != nil {
			v := d.FlinkApplicationConfigurationDescription.MonitoringConfigurationDescription
			if u.ConfigurationTypeUpdate != nil {
				v.ConfigurationType = u.ConfigurationTypeUpdate
			}

			if u.MetricsLevelUpdate != nil {
				v.MetricsLevel = u.MetricsLevelUpdate
			}

			if u.LogLevelUpdate != nil {
				v.LogLevel = u.LogLevelUpdate
			}
		}

		if u := f.ParallelismConfigurationUpdate; u != nil {
			v := d.FlinkApplicationConfigurationDescription.ParallelismConfigurationDescription
			if u.ConfigurationTypeUpdate != nil {
				v.ConfigurationType = u.ConfigurationTypeUpdate
			}

			if u.ParallelismUpdate != nil {
				v.Parallelism = u.ParallelismUpdate
			}

			if u.ParallelismPerKPUUpdate != nil {
				v.ParallelismPerKPU = u.ParallelismPerKPUUpdate
			}

			if u.AutoScalingEnabledUpdate != nil {
				v.AutoScalingEnabled = u.AutoScalingEnabledUpdate
			}
		}
	}

	for _, v := range u.VpcConfigurationUpdates {
		found := false
		for i, old := range d.VpcConfigurationDescriptions {
			if common.Value(old.VpcConfigurationId) != common.Value(v.VpcConfigurationId) {
				continue
			}

			config := &api.VpcConfiguration{SubnetIds: old.SubnetIds, SecurityGroupIds: old.SecurityGroupIds}
			if v.SubnetIdUpdates != nil {
				config.SubnetIds = v.SubnetIdUpdates
			}

			if v.SecurityGroupIdUpdates != nil {
				config.SecurityGroupIds = v.SecurityGroupIdUpdates
			}

			updated, e := s.vpc(tx, scope, config)
			if e != nil {
				return e
			}

			updated.VpcConfigurationId = old.VpcConfigurationId
			d.VpcConfigurationDescriptions[i] = *updated
			found = true
		}

		if !found {
			return missing()
		}
	}

	return validateConfiguration(d)
}
