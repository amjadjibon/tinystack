package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	"github.com/amjadjibon/tinystack/internal/aws/request"
)

const (
	mqSecurityGroups     = "SecurityGroups"
	mqEncryptionOptions  = "EncryptionOptions"
	mqBrokerType         = "AWS::AmazonMQ::Broker"
	mqBrokerName         = "BrokerName"
	mqPreviousBroker     = "MQPreviousBroker"
	mqPreviousProperties = "MQPreviousProperties"
)

// mqRecords is a narrow typed adapter for atomic configuration/tag updates and
// compensation. It is not an additional AWS operation or protocol endpoint.
type mqRecords interface {
	CreateBroker(context.Context, *api.CreateBrokerRequest) (*api.CreateBrokerResponse, error)
	DescribeBroker(context.Context, *api.DescribeBrokerRequest) (*api.DescribeBrokerResponse, error)
	DeleteBroker(context.Context, *api.DeleteBrokerRequest) (*api.DeleteBrokerResponse, error)
	CloudFormationUpdate(context.Context, *api.UpdateBrokerRequest, map[string]string, map[string]string) (
		*api.DescribeBrokerResponse, *api.DescribeBrokerResponse, error)
	CloudFormationRestore(context.Context, *api.DescribeBrokerResponse) (*api.DescribeBrokerResponse, error)
}

func (s *Service) UseMQ(records mqRecords) { s.mq = records }
func (c *client) mqContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "mq", Endpoint: c.endpoint})
}

func init() {
	register("AWS::AmazonMQ::Broker", &resourceType{
		properties: []string{
			mqBrokerName, "EngineType", memberEngineVersion, "HostInstanceType", "DeploymentMode",
			"PubliclyAccessible", "AutoMinorVersionUpgrade", "MaintenanceWindowStartTime", mqEncryptionOptions,
			"Logs", memberSubnetIDs, mqSecurityGroups, propertyUsers, memberTags,
		},
		replace: []string{
			mqBrokerName,
			"EngineType",
			"DeploymentMode",
			"PubliclyAccessible",
			mqEncryptionOptions,
			memberSubnetIDs,
		},
		attributes: []string{memberArn},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := mqParams(p)
			if e != nil {
				return "", nil, e
			}
			if c.mq == nil {
				return "", nil, validation("MQ record adapter is unavailable")
			}
			out, e := c.mq.CreateBroker(c.mqContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			return *out.BrokerId, map[string]any{memberArn: *out.BrokerArn}, nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := mqParams(p)
			if e != nil {
				return nil, e
			}
			if !reflect.DeepEqual(p[propertyUsers], r.Properties[propertyUsers]) {
				return nil, validation(
					"MQ in-place Users mutations are unsupported; initial credentials are write-only",
				)
			}
			// Removing references cannot invent a default group. Require an explicit
			// group list for private brokers, and reject clear attempts before writing.
			owned, e := mqCFTags(props(r.Properties))
			if e != nil {
				return nil, e
			}
			desired, e := mqCFTags(p)
			if e != nil {
				return nil, e
			}
			update := &api.UpdateBrokerRequest{
				BrokerId:                   new(r.Physical),
				EngineVersion:              in.EngineVersion,
				HostInstanceType:           in.HostInstanceType,
				AutoMinorVersionUpgrade:    in.AutoMinorVersionUpgrade,
				MaintenanceWindowStartTime: in.MaintenanceWindowStartTime,
				Logs:                       in.Logs,
				SecurityGroups:             in.SecurityGroups,
			}
			if update.AutoMinorVersionUpgrade == nil {
				update.AutoMinorVersionUpgrade = new(true)
			}
			if update.MaintenanceWindowStartTime == nil {
				update.MaintenanceWindowStartTime = &api.WeeklyStartTime{
					DayOfWeek: new("MONDAY"), TimeOfDay: new("00:00"), TimeZone: new("UTC"),
				}
			}
			if update.Logs == nil {
				update.Logs = &api.Logs{General: new(false)}
			}
			if *in.EngineType == "ACTIVEMQ" && update.Logs.Audit == nil {
				update.Logs.Audit = new(false)
			}
			if update.Logs.General == nil {
				update.Logs.General = new(false)
			}
			before, after, e := c.mq.CloudFormationUpdate(c.mqContext(ctx), update, owned, desired)
			if e != nil {
				return nil, e
			}
			snapshot, e := mqSnapshot(before)
			if e != nil {
				return nil, e
			}
			return map[string]any{
				memberArn: *after.BrokerArn, mqPreviousBroker: snapshot,
				mqPreviousProperties: mqEventProperties(props(r.Properties)),
			}, nil
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			_, e := c.mq.DeleteBroker(c.mqContext(ctx), &api.DeleteBrokerRequest{BrokerId: new(r.Physical)})
			if gone(e, "NotFoundException") {
				return nil
			}
			return e
		},
	})
}

func mqParams(p props) (*api.CreateBrokerRequest, error) {
	for prop, allowed := range map[string][]string{
		"Logs": {"General", "Audit"}, mqEncryptionOptions: {"UseAwsOwnedKey"},
		"MaintenanceWindowStartTime": {"DayOfWeek", "TimeOfDay", "TimeZone"},
	} {
		if object, ok := p[prop].(map[string]any); ok {
			for key := range object {
				if !slices.Contains(allowed, key) {
					return nil, validation("Unsupported MQ nested property %s.%s", prop, key)
				}
			}
		}
	}
	if users, ok := p[propertyUsers].([]any); ok {
		for _, value := range users {
			if user, ok := value.(map[string]any); ok {
				for key := range user {
					if key != "Username" && key != "Password" {
						return nil, validation("Unsupported MQ User property")
					}
				}
			}
		}
	}
	// JSON decoding is strict at every nesting level, preserving the generated
	// subset rather than silently discarding advanced user/encryption/log fields.
	tags, e := mqCFTags(p)
	if e != nil {
		return nil, e
	}
	params := maps.Clone(p)
	params[memberTags] = tags
	raw, e := json.Marshal(params)
	if e != nil {
		return nil, e
	}
	var in api.CreateBrokerRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&in); e != nil {
		return nil, validation("Invalid or unsupported MQ property type or nested member")
	}
	for _, value := range []*string{
		in.BrokerName, in.EngineType, in.EngineVersion, in.HostInstanceType, in.DeploymentMode,
	} {
		if value == nil || *value == "" {
			return nil, validation(
				"MQ BrokerName, EngineType, EngineVersion, HostInstanceType and DeploymentMode are required",
			)
		}
	}
	if in.PubliclyAccessible == nil || len(in.Users) == 0 {
		return nil, validation("MQ PubliclyAccessible and initial Users are required")
	}
	if !*in.PubliclyAccessible && (len(in.SubnetIds) == 0 || len(in.SecurityGroups) == 0) {
		return nil, validation("Private MQ brokers require explicit SubnetIds and SecurityGroups")
	}
	return &in, nil
}

func mqSnapshot(b *api.DescribeBrokerResponse) (map[string]any, error) {
	raw, e := json.Marshal(b)
	if e != nil {
		return nil, e
	}
	var out map[string]any
	e = json.Unmarshal(raw, &out)
	return out, e
}

// mqEventProperties redacts write-only passwords from resource event properties
// and rollback bookkeeping. Templates retain the shared CF storage behavior.
func mqEventProperties(p props) props {
	out := maps.Clone(p)
	if users, ok := p[propertyUsers].([]any); ok {
		clean := make([]any, 0, len(users))
		for _, value := range users {
			if user, ok := value.(map[string]any); ok {
				copyUser := maps.Clone(user)
				for key := range copyUser {
					if strings.EqualFold(key, "Password") {
						delete(copyUser, key)
					}
				}
				clean = append(clean, copyUser)
			} else {
				clean = append(clean, value)
			}
		}
		out[propertyUsers] = clean
	}
	return out
}

func mqRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || next.typ != mqBrokerType {
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

func mqRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	before, ok := r.Attributes[mqPreviousBroker].(map[string]any)
	previous, has := r.Attributes[mqPreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), mqEventProperties(p)) {
		return nil, false, nil
	}
	raw, e := json.Marshal(before)
	if e != nil {
		return nil, true, e
	}
	var b api.DescribeBrokerResponse
	if decodeErr := json.Unmarshal(raw, &b); decodeErr != nil {
		return nil, true, decodeErr
	}
	after, e := c.mq.CloudFormationRestore(c.mqContext(ctx), &b)
	if e != nil {
		return nil, true, e
	}
	return map[string]any{memberArn: *after.BrokerArn}, true, nil
}

func mqRead(ctx context.Context, c *client, id string) (map[string]any, error) {
	b, e := c.mq.DescribeBroker(c.mqContext(ctx), &api.DescribeBrokerRequest{BrokerId: new(id)})
	if e != nil {
		return nil, e
	}
	return map[string]any{memberArn: *b.BrokerArn}, nil
}

func mqCFTags(p props) (map[string]string, error) {
	out := map[string]string{}
	if !p.has(memberTags) {
		return out, nil
	}
	items, ok := p[memberTags].([]any)
	if !ok {
		return nil, validation("MQ Tags requires a list of Key/Value objects")
	}
	for _, v := range items {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, validation("Invalid MQ Tag")
		}
		for k := range m {
			if k != memberKey && k != memberValue {
				return nil, validation("Unsupported MQ Tag property %s", k)
			}
		}
		k, kok := m[memberKey].(string)
		val, vok := m[memberValue].(string)
		if !kok || !vok {
			return nil, validation("MQ tag Key and Value must be strings")
		}
		if _, dup := out[k]; dup {
			return nil, validation("Duplicate MQ tag key")
		}
		out[k] = val
	}
	return out, nil
}

// ProtectTemplates redacts template-bearing requests from diagnostics. MQ initial
// passwords may occur in these templates even though Smithy has no sensitive trait.
//
//nolint:gocritic // Startup decoration preserves the generated value descriptor contract.
func ProtectTemplates(d catalog.Service) catalog.Service {
	for _, name := range []string{
		"CreateStack", "UpdateStack", "CreateChangeSet", "ValidateTemplate", "GetTemplate", "GetTemplateSummary",
	} {
		op := d.Operations[name]
		op.Sensitive = true
		d.Operations[name] = op
	}
	return d
}

// mqRedactTemplate removes initial write-only passwords from GetTemplate.
// Stored templates are retained for replacement and rollback; the redacted
// retrieval is deliberately not a portable credential-bearing creation fixture.
func mqRedactTemplate(body string) string {
	doc, e := decode(body)
	if e != nil {
		return body
	}
	resources, _ := doc[keyResources].(map[string]any)
	changed := false
	for _, value := range resources {
		r, ok := value.(map[string]any)
		if !ok || r[memberType] != mqBrokerType {
			continue
		}
		p, ok := r[keyProperties].(map[string]any)
		if !ok {
			continue
		}
		clean := mqEventProperties(props(p))
		r[keyProperties] = clean
		changed = true
	}
	if !changed {
		return body
	}
	raw, e := json.Marshal(doc)
	if e != nil {
		return body
	}
	return string(raw)
}
