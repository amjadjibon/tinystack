package emr

import (
	"context"
	"net/url"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// Fixed labels from the EMR release guide; no AWS regional discovery calls are made.
// https://docs.aws.amazon.com/emr/latest/ReleaseGuide/emr-release-app-versions-7.x.html
var releases = strings.Fields(`emr-6.0.0 emr-6.1.0 emr-6.1.1 emr-6.2.0 emr-6.2.1 emr-6.3.0 emr-6.3.1
emr-6.4.0 emr-6.5.0 emr-6.6.0 emr-6.7.0 emr-6.8.0 emr-6.9.0 emr-6.10.0 emr-6.10.1 emr-6.11.0 emr-6.11.1
emr-6.12.0 emr-6.13.0 emr-6.14.0 emr-6.15.0 emr-7.0.0 emr-7.1.0 emr-7.2.0 emr-7.3.0 emr-7.4.0 emr-7.5.0
emr-7.6.0 emr-7.7.0 emr-7.8.0 emr-7.9.0 emr-7.10.0 emr-7.11.0 emr-7.12.0 emr-7.13.0 emr-7.14.0`)

func concurrency(v *int64) error {
	if v == nil || *v < 1 || *v > 256 {
		return invalid("StepConcurrencyLevel must be between 1 and 256.")
	}

	return nil
}

func (s *Service) references(ctx context.Context, scope request.Scope, in *api.RunJobFlowInput) error {
	role := common.Value(in.ServiceRole)
	if !strings.HasPrefix(role, "arn:") {
		role = "arn:aws:iam::" + scope.AccountID + ":role/" + role
	}

	r, ok, err := s.identity.RoleByARN(ctx, role)
	if err != nil {
		return err
	}

	if !ok || r.Account != scope.AccountID {
		return invalid("ServiceRole must name an existing role in this account.")
	}

	profile, ok, err := s.identity.InstanceProfileOf(ctx, scope.AccountID, common.Value(in.JobFlowRole))
	if err != nil {
		return err
	}

	if !ok || profile.RoleARN == "" {
		return invalid("JobFlowRole must name an existing instance profile with a role.")
	}

	if in.LogUri != nil {
		u, e := url.Parse(*in.LogUri)
		if e != nil || u.Scheme != "s3" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return invalid("LogUri must be an s3:// bucket location.")
		}

		if _, e = s.buckets.HeadBucket(
			ctx,
			&s3api.HeadBucketRequest{Bucket: new(u.Host), ExpectedBucketOwner: new(scope.AccountID)},
		); e != nil {
			return invalid("LogUri bucket must exist in this account: %s", e)
		}
	}

	return nil
}

func (s *Service) networkReferences(
	tx storage.ReadTx,
	scope request.Scope,
	in *api.JobFlowInstancesConfig,
) (string, *api.Ec2InstanceAttributes, error) {
	ids := slices.Clone(in.Ec2SubnetIds)
	if in.Ec2SubnetId != nil {
		if len(ids) > 0 {
			return "", nil, invalid("Ec2SubnetId and Ec2SubnetIds are mutually exclusive.")
		}

		ids = append(ids, *in.Ec2SubnetId)
	}

	attrs := &api.Ec2InstanceAttributes{
		Ec2KeyName:                     in.Ec2KeyName,
		Ec2SubnetId:                    in.Ec2SubnetId,
		RequestedEc2SubnetIds:          in.Ec2SubnetIds,
		EmrManagedMasterSecurityGroup:  in.EmrManagedMasterSecurityGroup,
		EmrManagedSlaveSecurityGroup:   in.EmrManagedSlaveSecurityGroup,
		ServiceAccessSecurityGroup:     in.ServiceAccessSecurityGroup,
		AdditionalMasterSecurityGroups: in.AdditionalMasterSecurityGroups,
		AdditionalSlaveSecurityGroups:  in.AdditionalSlaveSecurityGroups,
	}

	groups := append(slices.Clone(in.AdditionalMasterSecurityGroups), in.AdditionalSlaveSecurityGroups...)
	securityGroups := []*string{
		in.EmrManagedMasterSecurityGroup, in.EmrManagedSlaveSecurityGroup, in.ServiceAccessSecurityGroup,
	}

	for _, g := range securityGroups {
		if common.Value(g) != "" {
			groups = append(groups, *g)
		}
	}

	groups = slices.DeleteFunc(groups, func(id string) bool { return id == "" })
	cidr, vpc := "10.0.0.0/16", ""
	for i, id := range ids {
		sn, err := s.network.MountSubnet(tx, scope, id)
		if err != nil {
			return "", nil, invalid("Invalid subnet: %s", err)
		}

		if vpc != "" && vpc != common.Value(sn.VpcId) {
			return "", nil, invalid("Subnets must share a VPC.")
		}

		vpc = common.Value(sn.VpcId)
		if i == 0 {
			cidr = common.Value(sn.CidrBlock)
			attrs.Ec2SubnetId = new(id)
			attrs.Ec2AvailabilityZone = sn.AvailabilityZone
		}
	}

	if len(groups) > 0 {
		if vpc == "" {
			return "", nil, invalid("An explicit subnet is required with security groups.")
		}

		if _, err := s.network.MountGroups(tx, scope, vpc, groups); err != nil {
			return "", nil, invalid("Invalid security group: %s", err)
		}
	}

	return cidr, attrs, nil
}
