package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
)

const (
	mwaaEnvironmentType    = "AWS::MWAA::Environment"
	mwaaSecurityGroups     = "SecurityGroupIds"
	mwaaNetwork            = "NetworkConfiguration"
	mwaaVersion            = "AirflowVersion"
	mwaaPrevious           = "MWAAPreviousEnvironment"
	mwaaPreviousProperties = "MWAAPreviousProperties"
	mwaaCommitted          = "MWAACommittedRevision"
)

type mwaaRecords interface {
	CreateEnvironment(context.Context, *api.CreateEnvironmentInput) (*api.CreateEnvironmentOutput, error)
	GetEnvironment(context.Context, *api.GetEnvironmentInput) (*api.GetEnvironmentOutput, error)
	DeleteEnvironment(context.Context, *api.DeleteEnvironmentInput) (*api.DeleteEnvironmentOutput, error)
	CloudFormationUpdate(
		context.Context,
		*api.UpdateEnvironmentInput,
		map[string]string,
		map[string]string,
	) (*api.Environment, *api.Environment, string, error)
	CloudFormationRestore(context.Context, *api.Environment, string) (*api.Environment, error)
}

func (s *Service) UseMWAA(records mwaaRecords) { s.mwaa = records }
func (c *client) mwaaContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "mwaa", Endpoint: c.endpoint})
}

func mwaaAttributes(env *api.Environment) map[string]any { return map[string]any{memberArn: *env.Arn} }

func init() {
	register("AWS::MWAA::Environment", &resourceType{
		properties: []string{
			memberName, "ExecutionRoleArn", "SourceBucketArn", "DagS3Path", mwaaNetwork, mwaaVersion,
			"EnvironmentClass", "MinWorkers", "MaxWorkers", "Schedulers", memberTags,
		},
		replace: []string{memberName}, attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := mwaaParams(p)
			if e != nil {
				return "", nil, e
			}
			if c.mwaa == nil {
				return "", nil, validation("MWAA record adapter is unavailable")
			}
			_, e = c.mwaa.CreateEnvironment(c.mwaaContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			attrs, e := mwaaRead(ctx, c, *in.Name)
			return *in.Name, attrs, e
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := mwaaParams(p)
			if e != nil {
				return nil, e
			}
			if changeErr := mwaaUnsupportedChanges(props(r.Properties), p); changeErr != nil {
				return nil, changeErr
			}
			old, e := mwaaParams(props(r.Properties))
			if e != nil {
				return nil, e
			}
			// Optional removals restore documented defaults. Required configuration is
			// explicit locally, so removals fail in mwaaParams before the record write.
			if in.EnvironmentClass == nil {
				in.EnvironmentClass = new("mw1.small")
			}
			if in.MinWorkers == nil {
				in.MinWorkers = new(int64(1))
			}
			if in.MaxWorkers == nil {
				in.MaxWorkers = new(int64(10))
			}
			if in.Schedulers == nil {
				in.Schedulers = new(int64(2))
			}
			before, after, rev, e := c.mwaa.CloudFormationUpdate(c.mwaaContext(ctx), &api.UpdateEnvironmentInput{
				Name: new(
					r.Physical,
				),
				ExecutionRoleArn: in.ExecutionRoleArn,
				SourceBucketArn:  in.SourceBucketArn,
				DagS3Path:        in.DagS3Path,
				EnvironmentClass: in.EnvironmentClass,
				MinWorkers:       in.MinWorkers,
				MaxWorkers:       in.MaxWorkers,
				Schedulers:       in.Schedulers,
			}, old.Tags, common.CloneTags(in.Tags))
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
			attrs := mwaaAttributes(after)
			attrs[mwaaPrevious] = snapshot
			attrs[mwaaPreviousProperties] = r.Properties
			attrs[mwaaCommitted] = rev
			return attrs, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.mwaa.DeleteEnvironment(c.mwaaContext(ctx), &api.DeleteEnvironmentInput{Name: new(r.Physical)})
			if gone(e, "ResourceNotFoundException") {
				return nil
			}
			return e
		},
	})
}

func mwaaParams(p props) (*api.CreateEnvironmentInput, error) {
	if v, ok := p[mwaaNetwork].(map[string]any); ok {
		for k := range v {
			if !slices.Contains([]string{"SubnetIds", mwaaSecurityGroups}, k) {
				return nil, validation("Unsupported MWAA network member %s", k)
			}
		}
	}
	tags, e := mqCFTags(p)
	if e != nil {
		return nil, e
	}
	clean := maps.Clone(p)
	delete(clean, memberTags)
	delete(clean, memberName)
	raw, e := json.Marshal(clean)
	if e != nil {
		return nil, e
	}
	var in api.CreateEnvironmentInput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&in); e != nil {
		return nil, validation("Invalid or unsupported MWAA property type or member")
	}
	name, ok := p[memberName].(string)
	if !ok || name == "" {
		return nil, validation("MWAA Name is required")
	}
	in.Name = new(name)
	if common.Value(in.ExecutionRoleArn) == "" || common.Value(in.SourceBucketArn) == "" ||
		common.Value(in.DagS3Path) == "" ||
		common.Value(in.AirflowVersion) == "" ||
		in.NetworkConfiguration == nil {
		return nil, validation(
			"MWAA requires explicit execution role, source bucket, DAG prefix, network and Airflow version locally",
		)
	}
	for _, key := range []string{"EnvironmentClass", "MinWorkers", "MaxWorkers", "Schedulers"} {
		if v, has := p[key]; has && v == nil {
			return nil, validation("MWAA %s cannot be null; omit to restore its default", key)
		}
	}
	if len(tags) > 0 {
		in.Tags = tags
	}
	return &in, nil
}

func mwaaUnsupportedChanges(old, p props) error {
	if !reflect.DeepEqual(old[mwaaVersion], p[mwaaVersion]) {
		return validation("MWAA AirflowVersion changes/removals are unsupported")
	}
	a, _ := old[mwaaNetwork].(map[string]any)
	b, _ := p[mwaaNetwork].(map[string]any)
	if !reflect.DeepEqual(a[mwaaSecurityGroups], b[mwaaSecurityGroups]) {
		return validation("MWAA security-group updates are unsupported")
	}
	return nil
}

func mwaaReplaces(old, p props) bool {
	if !reflect.DeepEqual(old[memberName], p[memberName]) {
		return true
	}
	a, _ := old[mwaaNetwork].(map[string]any)
	b, _ := p[mwaaNetwork].(map[string]any)
	return !reflect.DeepEqual(a["SubnetIds"], b["SubnetIds"])
}

func mwaaRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || next.typ != mwaaEnvironmentType {
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

func mwaaRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	before, ok := r.Attributes[mwaaPrevious].(map[string]any)
	previous, has := r.Attributes[mwaaPreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	raw, e := json.Marshal(before)
	if e != nil {
		return nil, true, e
	}
	var b api.Environment
	if decodeErr := json.Unmarshal(raw, &b); decodeErr != nil {
		return nil, true, decodeErr
	}
	expected, ok := r.Attributes[mwaaCommitted].(string)
	if !ok {
		return nil, true, validation("Missing committed MWAA compensation revision")
	}
	after, e := c.mwaa.CloudFormationRestore(c.mwaaContext(ctx), &b, expected)
	if e != nil {
		return nil, true, e
	}
	return mwaaAttributes(after), true, nil
}

func mwaaRead(ctx context.Context, c *client, name string) (map[string]any, error) {
	out, e := c.mwaa.GetEnvironment(c.mwaaContext(ctx), &api.GetEnvironmentInput{Name: new(name)})
	if e != nil {
		return nil, e
	}
	return mwaaAttributes(out.Environment), nil
}
