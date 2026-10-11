package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	mskClusterType        = "AWS::MSK::Cluster"
	mskBrokerGroup        = "BrokerNodeGroupInfo"
	mskAZDistribution     = "BrokerAZDistribution"
	mskClientSubnets      = "ClientSubnets"
	mskMonitoring         = "EnhancedMonitoring"
	mskCurrentVersion     = "CurrentVersion"
	mskPrevious           = "MSKPreviousCluster"
	mskPreviousProperties = "MSKPreviousProperties"
	mskCommittedVersion   = "MSKCommittedVersion"
)

// mskRecords keeps configuration, final owned tags and the live rollback snapshot
// in one serializable write. These methods are not AWS protocol operations.
type mskRecords interface {
	CreateCluster(context.Context, *api.CreateClusterRequest) (*api.CreateClusterResponse, error)
	DescribeCluster(context.Context, *api.DescribeClusterRequest) (*api.DescribeClusterResponse, error)
	DeleteCluster(context.Context, *api.DeleteClusterRequest) (*api.DeleteClusterResponse, error)
	CloudFormationUpdate(
		context.Context,
		*api.UpdateMonitoringRequest,
		map[string]string,
		map[string]string,
	) (*api.ClusterInfo, *api.ClusterInfo, error)
	CloudFormationRestore(context.Context, *api.ClusterInfo, string) (*api.ClusterInfo, error)
}

func (s *Service) UseMSK(records mskRecords) { s.msk = records }
func (c *client) mskContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "kafka", Endpoint: c.endpoint})
}

func mskAttributes(b *api.ClusterInfo) map[string]any {
	return map[string]any{memberArn: *b.ClusterArn, mskCurrentVersion: *b.CurrentVersion}
}

func init() {
	register("AWS::MSK::Cluster", &resourceType{
		properties: []string{
			memberClusterName,
			mskBrokerGroup,
			"KafkaVersion",
			"NumberOfBrokerNodes",
			mskMonitoring,
			memberTags,
		},
		replace:    []string{memberClusterName},
		attributes: []string{memberArn, mskCurrentVersion},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := mskParams(p)
			if e != nil {
				return "", nil, e
			}
			if c.msk == nil {
				return "", nil, validation("MSK record adapter is unavailable")
			}
			out, e := c.msk.CreateCluster(c.mskContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			attrs, e := mskRead(ctx, c, *out.ClusterArn)
			return *out.ClusterArn, attrs, e
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := mskParams(p)
			if e != nil {
				return nil, e
			}
			if changeErr := mskUnsupportedChanges(props(r.Properties), p); changeErr != nil {
				return nil, changeErr
			}
			live, e := c.msk.DescribeCluster(
				c.mskContext(ctx),
				&api.DescribeClusterRequest{ClusterArn: new(r.Physical)},
			)
			if e != nil {
				return nil, e
			}
			old, e := mskParams(props(r.Properties))
			if e != nil {
				return nil, e
			}
			monitor := in.EnhancedMonitoring
			if monitor == nil {
				monitor = new("DEFAULT")
			}
			before, after, e := c.msk.CloudFormationUpdate(
				c.mskContext(ctx),
				&api.UpdateMonitoringRequest{
					ClusterArn:         new(r.Physical),
					CurrentVersion:     live.ClusterInfo.CurrentVersion,
					EnhancedMonitoring: monitor,
				},
				old.Tags,
				in.Tags,
			)
			if e != nil {
				return nil, e
			}
			raw, e := json.Marshal(before)
			if e != nil {
				return nil, e
			}
			var snapshot map[string]any
			if e := json.Unmarshal(raw, &snapshot); e != nil {
				return nil, e
			}
			attrs := mskAttributes(after)
			attrs[mskPrevious] = snapshot
			attrs[mskPreviousProperties] = r.Properties
			attrs[mskCommittedVersion] = *after.CurrentVersion
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.msk.DeleteCluster(c.mskContext(ctx), &api.DeleteClusterRequest{ClusterArn: new(r.Physical)})
			if gone(e, "NotFoundException") {
				return nil
			}
			return e
		},
	})
}

func mskParams(p props) (*api.CreateClusterRequest, error) {
	// Validate property spelling even for excluded false/null/empty values. CF's
	// EBSStorageInfo spelling maps to the modeled EbsStorageInfo member.
	for name, allowed := range map[string][]string{
		mskBrokerGroup: {mskAZDistribution, mskClientSubnets, "InstanceType", mqSecurityGroups, "StorageInfo"},
	} {
		if v, ok := p[name].(map[string]any); ok {
			for k := range v {
				if !slices.Contains(allowed, k) {
					return nil, validation("Unsupported MSK nested property %s.%s", name, k)
				}
			}
		}
	}
	if group, ok := p[mskBrokerGroup].(map[string]any); ok {
		if st, ok := group["StorageInfo"].(map[string]any); ok {
			for k := range st {
				if k != "EBSStorageInfo" {
					return nil, validation("Unsupported MSK StorageInfo property %s", k)
				}
			}
			if ebs, ok := st["EBSStorageInfo"].(map[string]any); ok {
				for k := range ebs {
					if k != "VolumeSize" {
						return nil, validation("Unsupported MSK EBSStorageInfo property %s", k)
					}
				}
			}
		}
	}
	if p.has(memberTags) {
		v, ok := p[memberTags].(map[string]any)
		if !ok {
			return nil, validation("MSK Tags requires an object of string values")
		}
		for _, value := range v {
			if _, ok := value.(string); !ok {
				return nil, validation("MSK tag values must be strings")
			}
		}
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	var in api.CreateClusterRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&in); e != nil {
		return nil, validation("Invalid or unsupported MSK property type or nested member")
	}
	if common.Value(in.ClusterName) == "" || common.Value(in.KafkaVersion) == "" || in.NumberOfBrokerNodes == nil ||
		in.BrokerNodeGroupInfo == nil {
		return nil, validation(
			"MSK ClusterName, KafkaVersion, NumberOfBrokerNodes and BrokerNodeGroupInfo are required",
		)
	}
	b := in.BrokerNodeGroupInfo
	if b.InstanceType == nil || len(b.ClientSubnets) == 0 || len(b.SecurityGroups) == 0 || b.StorageInfo == nil ||
		b.StorageInfo.EbsStorageInfo == nil ||
		b.StorageInfo.EbsStorageInfo.VolumeSize == nil {
		return nil, validation("MSK requires explicit broker instance, subnets, groups and EBS volume size locally")
	}
	if in.Tags == nil {
		in.Tags = map[string]string{}
	}
	return &in, nil
}

func mskUnsupportedChanges(old, p props) error {
	for _, key := range []string{"KafkaVersion", "NumberOfBrokerNodes"} {
		if !reflect.DeepEqual(old[key], p[key]) {
			return validation("MSK %s mutations require an unsupported update API", key)
		}
	}
	a, _ := old[mskBrokerGroup].(map[string]any)
	b, _ := p[mskBrokerGroup].(map[string]any)
	for _, key := range []string{"InstanceType", "StorageInfo"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			return validation("MSK %s mutations/removal require an unsupported update API", key)
		}
	}
	// Removing explicit immutable choices cannot infer replacement defaults.
	for _, key := range []string{mskClientSubnets, mqSecurityGroups, mskAZDistribution} {
		if _, has := a[key]; has {
			if _, has = b[key]; !has {
				return validation("Removing MSK %s is unsupported", key)
			}
		}
	}
	return nil
}

func mskReplaces(old, p props) bool {
	if !reflect.DeepEqual(old[memberClusterName], p[memberClusterName]) {
		return true
	}
	a, _ := old[mskBrokerGroup].(map[string]any)
	b, _ := p[mskBrokerGroup].(map[string]any)
	for _, key := range []string{mskClientSubnets, mqSecurityGroups, mskAZDistribution} {
		if !reflect.DeepEqual(a[key], b[key]) {
			return true
		}
	}
	return false
}

func mskRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || next.typ != mskClusterType {
		return "", nil, false
	}
	for _, old := range st.Replaced {
		if old.Logical == next.logical && old.Resource.Type == next.typ &&
			reflect.DeepEqual(props(old.Resource.Properties), next.props) {
			return old.Resource.Physical, old.Resource.Attributes, true
		}
	}
	return "", nil, false
}

func mskRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	before, ok := r.Attributes[mskPrevious].(map[string]any)
	previous, has := r.Attributes[mskPreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	raw, e := json.Marshal(before)
	if e != nil {
		return nil, true, e
	}
	var b api.ClusterInfo
	if decodeErr := json.Unmarshal(raw, &b); decodeErr != nil {
		return nil, true, decodeErr
	}
	expected, ok := r.Attributes[mskCommittedVersion].(string)
	if !ok {
		return nil, true, validation("Missing committed MSK revision for rollback")
	}
	after, e := c.msk.CloudFormationRestore(c.mskContext(ctx), &b, expected)
	if e != nil {
		return nil, true, e
	}
	return mskAttributes(after), true, nil
}

func mskRead(ctx context.Context, c *client, id string) (map[string]any, error) {
	out, e := c.msk.DescribeCluster(c.mskContext(ctx), &api.DescribeClusterRequest{ClusterArn: new(id)})
	if e != nil {
		return nil, e
	}
	return mskAttributes(out.ClusterInfo), nil
}
