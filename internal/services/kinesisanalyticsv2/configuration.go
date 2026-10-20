package kinesisanalyticsv2

import (
	"context"
	"crypto/md5" // #nosec G501 -- AWS exposes an MD5 code fingerprint, never used for security.
	"fmt"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) codeDescription(
	ctx context.Context,
	c *api.ApplicationCodeConfiguration,
) (*api.ApplicationCodeConfigurationDescription, error) {
	if c == nil {
		return nil, nil //nolint:nilnil // Omitted optional code configuration has no description.
	}

	if c.CodeContent == nil {
		return nil, invalid("CodeContent is required.")
	}

	if !slices.Contains([]string{"PLAINTEXT", "ZIPFILE"}, common.Value(c.CodeContentType)) {
		return nil, invalid("Invalid CodeContentType.")
	}

	content := c.CodeContent
	count := 0
	var data []byte
	d := &api.CodeContentDescription{}
	if content.TextContent != nil {
		count++
		data = []byte(*content.TextContent)
		d.TextContent = content.TextContent
		if len(data) > 102400 || *c.CodeContentType != "PLAINTEXT" {
			return nil, invalid("TextContent requires PLAINTEXT and at most 102400 bytes.")
		}
	}

	if content.ZipFileContent != nil {
		count++
		data = content.ZipFileContent
		if len(data) > 50<<20 || *c.CodeContentType != "ZIPFILE" {
			return nil, invalid("ZipFileContent requires ZIPFILE and at most 52428800 bytes.")
		}
	}

	if loc := content.S3ContentLocation; loc != nil {
		count++
		bucket, ok := strings.CutPrefix(common.Value(loc.BucketARN), "arn:aws:s3:::")
		if !ok || bucket == "" || strings.Contains(bucket, "/") || common.Value(loc.FileKey) == "" {
			return nil, invalid("Invalid S3 code location.")
		}

		var err error
		data, err = s.source.Object(ctx, bucket, *loc.FileKey, common.Value(loc.ObjectVersion))
		if err != nil {
			return nil, invalid("S3 code object must exist in this account and Region: %s", err)
		}

		if len(data) > 50<<20 {
			return nil, invalid("S3 code object exceeds 50 MiB.")
		}

		d.S3ApplicationCodeLocationDescription = &api.S3ApplicationCodeLocationDescription{
			BucketARN:     loc.BucketARN,
			FileKey:       loc.FileKey,
			ObjectVersion: loc.ObjectVersion,
		}
	}

	if count != 1 {
		return nil, invalid("Specify exactly one code content source.")
	}

	d.CodeSize = new(int64(len(data)))
	d.CodeMD5 = new(fmt.Sprintf("%x", md5.Sum(data))) // #nosec G401 -- wire-compatible metadata fingerprint only.
	return &api.ApplicationCodeConfigurationDescription{
		CodeContentType:        c.CodeContentType,
		CodeContentDescription: d,
	}, nil
}

func (s *Service) configure(tx storage.ReadTx, scope request.Scope, r *record, c *api.ApplicationConfiguration) error {
	d := r.Detail.ApplicationConfigurationDescription
	d.ApplicationSnapshotConfigurationDescription = &api.ApplicationSnapshotConfigurationDescription{
		SnapshotsEnabled: new(true),
	}

	d.FlinkApplicationConfigurationDescription = &api.FlinkApplicationConfigurationDescription{
		CheckpointConfigurationDescription: &api.CheckpointConfigurationDescription{
			ConfigurationType:          new(configurationDefault),
			CheckpointingEnabled:       new(true),
			CheckpointInterval:         new(int64(60000)),
			MinPauseBetweenCheckpoints: new(int64(5000)),
		},
		MonitoringConfigurationDescription: &api.MonitoringConfigurationDescription{
			ConfigurationType: new(configurationDefault),
			MetricsLevel:      new("APPLICATION"),
			LogLevel:          new("INFO"),
		},
		ParallelismConfigurationDescription: &api.ParallelismConfigurationDescription{
			ConfigurationType:  new(configurationDefault),
			Parallelism:        new(int64(1)),
			ParallelismPerKPU:  new(int64(1)),
			CurrentParallelism: new(int64(1)),
			AutoScalingEnabled: new(false),
		},
	}

	if c == nil {
		return nil
	}

	if c.ApplicationSnapshotConfiguration != nil {
		d.ApplicationSnapshotConfigurationDescription.SnapshotsEnabled = c.ApplicationSnapshotConfiguration.SnapshotsEnabled
		if d.ApplicationSnapshotConfigurationDescription.SnapshotsEnabled == nil {
			return invalid("SnapshotsEnabled is required.")
		}
	}

	if c.EnvironmentProperties != nil {
		d.EnvironmentPropertyDescriptions = &api.EnvironmentPropertyDescriptions{
			PropertyGroupDescriptions: c.EnvironmentProperties.PropertyGroups,
		}
	}

	if f := c.FlinkApplicationConfiguration; f != nil {
		out := d.FlinkApplicationConfigurationDescription
		if f.CheckpointConfiguration != nil {
			out.CheckpointConfigurationDescription = (*api.CheckpointConfigurationDescription)(
				f.CheckpointConfiguration,
			)
		}

		if f.MonitoringConfiguration != nil {
			out.MonitoringConfigurationDescription = (*api.MonitoringConfigurationDescription)(
				f.MonitoringConfiguration,
			)
		}

		if p := f.ParallelismConfiguration; p != nil {
			out.ParallelismConfigurationDescription = &api.ParallelismConfigurationDescription{
				ConfigurationType:  p.ConfigurationType,
				Parallelism:        p.Parallelism,
				ParallelismPerKPU:  p.ParallelismPerKPU,
				CurrentParallelism: p.Parallelism,
				AutoScalingEnabled: p.AutoScalingEnabled,
			}
		}
	}

	if len(c.VpcConfigurations) > 1 {
		return invalid("Only one VPC configuration is allowed.")
	}

	for _, v := range c.VpcConfigurations {
		vd, err := s.vpc(tx, scope, &v)
		if err != nil {
			return err
		}

		d.VpcConfigurationDescriptions = append(d.VpcConfigurationDescriptions, *vd)
	}

	return validateConfiguration(d)
}

func validateConfiguration(d *api.ApplicationConfigurationDescription) error {
	if e := validateProperties(d.EnvironmentPropertyDescriptions); e != nil {
		return e
	}

	f := d.FlinkApplicationConfigurationDescription
	c := f.CheckpointConfigurationDescription
	m := f.MonitoringConfigurationDescription
	p := f.ParallelismConfigurationDescription
	for _, t := range []*string{c.ConfigurationType, m.ConfigurationType, p.ConfigurationType} {
		if !slices.Contains([]string{configurationDefault, "CUSTOM"}, common.Value(t)) {
			return invalid("ConfigurationType must be DEFAULT or CUSTOM.")
		}
	}

	if *c.ConfigurationType == configurationDefault {
		c.CheckpointingEnabled = new(true)
		c.CheckpointInterval = new(int64(60000))
		c.MinPauseBetweenCheckpoints = new(int64(5000))
	}

	if *m.ConfigurationType == configurationDefault {
		m.LogLevel = new("INFO")
		m.MetricsLevel = new("APPLICATION")
	}

	if *p.ConfigurationType == configurationDefault {
		p.Parallelism = new(int64(1))
		p.ParallelismPerKPU = new(int64(1))
		p.AutoScalingEnabled = new(false)
	}

	if c.CheckpointingEnabled == nil {
		c.CheckpointingEnabled = new(true)
	}

	if c.CheckpointInterval == nil {
		c.CheckpointInterval = new(int64(60000))
	}

	if c.MinPauseBetweenCheckpoints == nil {
		c.MinPauseBetweenCheckpoints = new(int64(5000))
	}

	if m.LogLevel == nil {
		m.LogLevel = new("INFO")
	}

	if m.MetricsLevel == nil {
		m.MetricsLevel = new("APPLICATION")
	}

	if p.Parallelism == nil {
		p.Parallelism = new(int64(1))
	}

	if p.ParallelismPerKPU == nil {
		p.ParallelismPerKPU = new(int64(1))
	}

	if p.AutoScalingEnabled == nil {
		p.AutoScalingEnabled = new(false)
	}

	p.CurrentParallelism = p.Parallelism
	if *c.CheckpointInterval < 1 || *c.MinPauseBetweenCheckpoints < 0 || *p.Parallelism < 1 ||
		*p.ParallelismPerKPU < 1 {
		return invalid("Invalid checkpoint or parallelism configuration.")
	}

	if !slices.Contains([]string{"INFO", "WARN", "ERROR", "DEBUG"}, common.Value(m.LogLevel)) ||
		!slices.Contains([]string{"APPLICATION", "TASK", "OPERATOR", "PARALLELISM"}, common.Value(m.MetricsLevel)) {
		return invalid("Invalid monitoring configuration.")
	}

	return nil
}

func validateProperties(e *api.EnvironmentPropertyDescriptions) error {
	if e == nil {
		return nil
	}

	if len(e.PropertyGroupDescriptions) > 50 {
		return invalid("At most 50 property groups are allowed.")
	}

	seen := map[string]bool{}
	for _, g := range e.PropertyGroupDescriptions {
		id := common.Value(g.PropertyGroupId)
		if !namePattern.MatchString(id) || len(id) > 50 || seen[id] ||
			len(g.PropertyMap) < 1 || len(g.PropertyMap) > 50 {
			return invalid("Invalid or duplicate property group.")
		}

		seen[id] = true
		for k, v := range g.PropertyMap {
			if len(k) < 1 || len(k) > 2048 || len(v) < 1 || len(v) > 2048 {
				return invalid("Property keys and values require 1-2048 characters.")
			}
		}
	}

	return nil
}

func (s *Service) vpc(
	tx storage.ReadTx,
	scope request.Scope,
	v *api.VpcConfiguration,
) (*api.VpcConfigurationDescription, error) {
	if v == nil || len(v.SubnetIds) < 1 || len(v.SubnetIds) > 16 || len(v.SecurityGroupIds) < 1 ||
		len(v.SecurityGroupIds) > 5 {
		return nil, invalid("VPC configuration requires 1-16 subnets and 1-5 security groups.")
	}

	var vpcID string
	seen := map[string]bool{}
	for _, id := range v.SubnetIds {
		if seen[id] {
			return nil, invalid("Duplicate subnet.")
		}

		seen[id] = true
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return nil, invalid("Subnet must exist in this account and Region: %s", err)
		}

		if vpcID != "" && vpcID != common.Value(sn.VpcId) {
			return nil, invalid("Subnets must belong to the same VPC.")
		}

		vpcID = common.Value(sn.VpcId)
	}

	if _, err := s.network.MountGroups(tx, scope, vpcID, v.SecurityGroupIds); err != nil {
		return nil, invalid("Invalid security group: %s", err)
	}

	return &api.VpcConfigurationDescription{
		VpcConfigurationId: new("vpc-" + common.RandomID(16)),
		VpcId:              new(vpcID),
		SubnetIds:          v.SubnetIds,
		SecurityGroupIds:   v.SecurityGroupIds,
	}, nil
}

func checkRun(tx storage.ReadTx, scope request.Scope, r *record, c *api.RunConfiguration) error {
	if c == nil {
		return invalid("RunConfiguration is required.")
	}

	if a := c.ApplicationRestoreConfiguration; a != nil {
		switch common.Value(a.ApplicationRestoreType) {
		case "SKIP_RESTORE_FROM_SNAPSHOT", "RESTORE_FROM_LATEST_SNAPSHOT":
			if a.SnapshotName != nil {
				return invalid("SnapshotName requires RESTORE_FROM_CUSTOM_SNAPSHOT.")
			}
		case "RESTORE_FROM_CUSTOM_SNAPSHOT":
			if _, err := readSnapshot(tx, scope, r.Detail.ApplicationName, a.SnapshotName); err != nil {
				return err
			}
		default:
			return invalid("Invalid ApplicationRestoreType.")
		}
	}

	r.Detail.ApplicationConfigurationDescription.RunConfigurationDescription = &api.RunConfigurationDescription{
		ApplicationRestoreConfigurationDescription: c.ApplicationRestoreConfiguration,
		FlinkRunConfigurationDescription:           c.FlinkRunConfiguration,
	}

	return nil
}
