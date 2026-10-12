package cloudformation

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/services/transfer"
)

const (
	transferServerType         = "AWS::Transfer::Server"
	transferUserType           = "AWS::Transfer::User"
	transferPrevious           = "TransferPreviousRecord"
	transferPreviousProperties = "TransferPreviousProperties"
	transferCommitted          = "TransferCommittedRevision"
	transferServerID           = "ServerId"
	transferUserName           = "UserName"
	transferBanner             = "PreAuthenticationLoginBanner"
	transferPolicy             = "SecurityPolicyName"
	transferHome               = "HomeDirectory"
	transferHomeType           = "HomeDirectoryType"
)

type transferRecords interface {
	CreateServer(context.Context, *api.CreateServerRequest) (*api.CreateServerResponse, error)
	DescribeServer(context.Context, *api.DescribeServerRequest) (*api.DescribeServerResponse, error)
	DeleteServer(context.Context, *api.DeleteServerRequest) (*api.DeleteServerOutput, error)
	CreateUser(context.Context, *api.CreateUserRequest) (*api.CreateUserResponse, error)
	DescribeUser(context.Context, *api.DescribeUserRequest) (*api.DescribeUserResponse, error)
	DeleteUser(context.Context, *api.DeleteUserRequest) (*api.DeleteUserOutput, error)
	CloudFormationUpdateServer(
		context.Context,
		*api.UpdateServerRequest,
		map[string]string,
		map[string]string,
	) (*transfer.Snapshot, *transfer.Snapshot, error)
	CloudFormationUpdateUser(
		context.Context,
		*api.UpdateUserRequest,
		map[string]string,
		map[string]string,
	) (*transfer.Snapshot, *transfer.Snapshot, error)
	CloudFormationRestore(context.Context, *transfer.Snapshot, string) (*transfer.Snapshot, error)
}

func (s *Service) UseTransfer(records transferRecords) { s.transfer = records }
func (c *client) transferContext(ctx context.Context) context.Context {
	return request.WithMetadata(ctx, request.Metadata{Scope: c.scope, Service: "transfer", Endpoint: c.endpoint})
}

func isTransferResource(typ string) bool { return typ == transferServerType || typ == transferUserType }

func transferAttributes(r *transfer.Snapshot) map[string]any {
	if r.User != nil {
		return map[string]any{memberArn: *r.User.Arn, transferServerID: r.ServerID, transferUserName: *r.User.UserName}
	}
	return map[string]any{memberArn: *r.Server.Arn, transferServerID: r.ServerID}
}

func transferDecode(p props, out any) error {
	if tags, has := p[memberTags]; has {
		list, ok := tags.([]any)
		if !ok || len(list) == 0 {
			return validation("Transfer Tags requires 1-50 Key/Value objects")
		}
		for _, value := range list {
			tag, ok := value.(map[string]any)
			if !ok {
				return validation("Transfer Tag requires a Key/Value object")
			}
			for key := range tag {
				if key != memberKey && key != memberValue {
					return validation("Unsupported Transfer Tag member %s", key)
				}
			}
		}
	}

	for k, v := range p {
		if v == nil {
			return validation("Transfer property %s cannot be null", k)
		}
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return validation("Invalid or unsupported Transfer property member/type")
	}
	return nil
}

func transferServerParams(p props) (*api.CreateServerRequest, error) {
	var in api.CreateServerRequest
	if e := transferDecode(p, &in); e != nil {
		return nil, e
	}
	if in.Domain == nil {
		in.Domain = new("S3")
	}
	if in.IdentityProviderType == nil {
		in.IdentityProviderType = new("SERVICE_MANAGED")
	}
	if in.SecurityPolicyName == nil {
		in.SecurityPolicyName = new("TransferSecurityPolicy-2018-11")
	}
	if _, has := p[memberTags]; has && len(in.Tags) == 0 {
		return nil, validation("Explicit Transfer Tags requires 1-50 pairs")
	}
	e := transfer.ValidateServerConfiguration(
		&api.DescribedServer{
			Domain:                       in.Domain,
			EndpointType:                 in.EndpointType,
			IdentityProviderType:         in.IdentityProviderType,
			Protocols:                    in.Protocols,
			SecurityPolicyName:           in.SecurityPolicyName,
			PreAuthenticationLoginBanner: in.PreAuthenticationLoginBanner,
			Tags:                         in.Tags,
		},
	)
	return &in, e
}

func transferUserParams(p props, scope request.Scope) (*api.CreateUserRequest, error) {
	var in api.CreateUserRequest
	if e := transferDecode(p, &in); e != nil {
		return nil, e
	}
	if in.HomeDirectoryType == nil {
		in.HomeDirectoryType = new("PATH")
	}
	if in.ServerId == nil || in.UserName == nil {
		return nil, validation("Transfer User requires ServerId and UserName")
	}
	if _, has := p[memberTags]; has && len(in.Tags) == 0 {
		return nil, validation("Explicit Transfer Tags requires 1-50 pairs")
	}
	e := transfer.ValidateUserConfiguration(
		&api.DescribedUser{
			UserName:          in.UserName,
			Role:              in.Role,
			HomeDirectory:     in.HomeDirectory,
			HomeDirectoryType: in.HomeDirectoryType,
			Tags:              in.Tags,
		},
		scope,
	)
	return &in, e
}

func transferCFTags(tags []api.Tag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		out[*t.Key] = *t.Value
	}
	return out
}

func transferPreflight(typ string, old, p props, scope request.Scope) error {
	if typ == transferServerType {
		if _, e := transferServerParams(p); e != nil {
			return e
		}
		if _, was := old[transferBanner]; was {
			if _, has := p[transferBanner]; !has {
				return validation("Removing Transfer banner is unsupported; use an explicit empty string to clear it")
			}
		}
	} else {
		if _, e := transferUserParams(p, scope); e != nil {
			return e
		}
	}
	return nil
}

func transferSnapshotAttributes(r *resource, before, after *transfer.Snapshot) (map[string]any, error) {
	raw, e := json.Marshal(before)
	if e != nil {
		return nil, e
	}
	var snapshot map[string]any
	if decodeErr := json.Unmarshal(raw, &snapshot); decodeErr != nil {
		return nil, decodeErr
	}
	attrs := transferAttributes(after)
	attrs[transferPrevious] = snapshot
	attrs[transferPreviousProperties] = maps.Clone(r.Properties)
	attrs[transferCommitted] = after.Revision
	return attrs, nil
}

func init() {
	register("AWS::Transfer::Server", &resourceType{
		properties: []string{
			"Domain",
			"EndpointType",
			"IdentityProviderType",
			"Protocols",
			transferPolicy,
			transferBanner,
			memberTags,
		},
		replace:    []string{"Domain"},
		attributes: []string{memberArn, transferServerID},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := transferServerParams(p)
			if e != nil {
				return "", nil, e
			}
			if c.transfer == nil {
				return "", nil, validation("Transfer record adapter is unavailable")
			}
			out, e := c.transfer.CreateServer(c.transferContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			r, e := c.transfer.DescribeServer(
				c.transferContext(ctx),
				&api.DescribeServerRequest{ServerId: out.ServerId},
			)
			if e != nil {
				return "", nil, e
			}
			return *r.Server.Arn, transferAttributes(&transfer.Snapshot{Server: r.Server, ServerID: *out.ServerId}), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			if e := transferPreflight(r.Type, props(r.Properties), p, c.scope); e != nil {
				return nil, e
			}
			in, e := transferServerParams(p)
			if e != nil {
				return nil, e
			}
			old, e := transferServerParams(props(r.Properties))
			if e != nil {
				return nil, e
			}
			id := r.Attributes[transferServerID].(string)
			before, after, e := c.transfer.CloudFormationUpdateServer(
				c.transferContext(ctx),
				&api.UpdateServerRequest{
					ServerId:                     new(id),
					EndpointType:                 in.EndpointType,
					IdentityProviderType:         in.IdentityProviderType,
					Protocols:                    in.Protocols,
					SecurityPolicyName:           in.SecurityPolicyName,
					PreAuthenticationLoginBanner: in.PreAuthenticationLoginBanner,
				},
				transferCFTags(old.Tags),
				transferCFTags(in.Tags),
			)
			if e != nil {
				return nil, e
			}
			return transferSnapshotAttributes(r, before, after)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			id := r.Attributes[transferServerID].(string)
			_, e := c.transfer.DeleteServer(c.transferContext(ctx), &api.DeleteServerRequest{ServerId: new(id)})
			if gone(e, "ResourceNotFoundException") {
				return nil
			}
			return e
		},
	})
	register("AWS::Transfer::User", &resourceType{
		properties: []string{transferServerID, transferUserName, "Role", transferHome, transferHomeType, memberTags},
		replace: []string{
			transferServerID,
			transferUserName,
		},
		attributes: []string{memberArn, transferServerID, transferUserName},
		create: func(ctx context.Context, c *client, p props) (string, map[string]any, error) {
			in, e := transferUserParams(p, c.scope)
			if e != nil {
				return "", nil, e
			}
			if c.transfer == nil {
				return "", nil, validation("Transfer record adapter is unavailable")
			}
			out, e := c.transfer.CreateUser(c.transferContext(ctx), in)
			if e != nil {
				return "", nil, e
			}
			r, e := c.transfer.DescribeUser(
				c.transferContext(ctx),
				&api.DescribeUserRequest{ServerId: out.ServerId, UserName: out.UserName},
			)
			if e != nil {
				return "", nil, e
			}
			return *r.User.Arn, transferAttributes(&transfer.Snapshot{User: r.User, ServerID: *out.ServerId}), nil
		},
		update: func(ctx context.Context, c *client, r *resource, p props) (map[string]any, error) {
			in, e := transferUserParams(p, c.scope)
			if e != nil {
				return nil, e
			}
			old, e := transferUserParams(props(r.Properties), c.scope)
			if e != nil {
				return nil, e
			}
			id, name := r.Attributes[transferServerID].(string), r.Attributes[transferUserName].(string)
			before, after, e := c.transfer.CloudFormationUpdateUser(
				c.transferContext(ctx),
				&api.UpdateUserRequest{
					ServerId:          new(id),
					UserName:          new(name),
					Role:              in.Role,
					HomeDirectory:     in.HomeDirectory,
					HomeDirectoryType: in.HomeDirectoryType,
				},
				transferCFTags(old.Tags),
				transferCFTags(in.Tags),
			)
			if e != nil {
				return nil, e
			}
			return transferSnapshotAttributes(r, before, after)
		},
		remove: func(ctx context.Context, c *client, r *resource) error {
			id, name := r.Attributes[transferServerID].(string), r.Attributes[transferUserName].(string)
			_, e := c.transfer.DeleteUser(
				c.transferContext(ctx),
				&api.DeleteUserRequest{ServerId: new(id), UserName: new(name)},
			)
			if gone(e, "ResourceNotFoundException") {
				return nil
			}
			return e
		},
	})
}

func transferRestoreReplacement(st *stack, next *step) (physical string, attributes map[string]any, found bool) {
	if !st.Op.Rollback || !isTransferResource(next.typ) {
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

func transferRead(ctx context.Context, c *client, r *resource) (map[string]any, error) {
	id := r.Attributes[transferServerID].(string)
	if r.Type == transferServerType {
		out, e := c.transfer.DescribeServer(c.transferContext(ctx), &api.DescribeServerRequest{ServerId: new(id)})
		if e != nil {
			return nil, e
		}
		return transferAttributes(&transfer.Snapshot{Server: out.Server, ServerID: id}), nil
	}
	name := r.Attributes[transferUserName].(string)
	out, e := c.transfer.DescribeUser(
		c.transferContext(ctx),
		&api.DescribeUserRequest{ServerId: new(id), UserName: new(name)},
	)
	if e != nil {
		return nil, e
	}
	return transferAttributes(&transfer.Snapshot{User: out.User, ServerID: id}), nil
}

func transferRestoreUpdate(
	ctx context.Context,
	c *client,
	r *resource,
	p props,
) (attributes map[string]any, found bool, err error) {
	b, ok := r.Attributes[transferPrevious].(map[string]any)
	previous, has := r.Attributes[transferPreviousProperties].(map[string]any)
	if !ok || !has || !reflect.DeepEqual(props(previous), p) {
		return nil, false, nil
	}
	raw, e := json.Marshal(b)
	if e != nil {
		return nil, true, e
	}
	var snapshot transfer.Snapshot
	if decodeErr := json.Unmarshal(raw, &snapshot); decodeErr != nil {
		return nil, true, decodeErr
	}
	rev, ok := r.Attributes[transferCommitted].(string)
	if !ok || strings.TrimSpace(rev) == "" {
		return nil, true, validation("Missing Transfer compensation revision")
	}
	after, e := c.transfer.CloudFormationRestore(c.transferContext(ctx), &snapshot, rev)
	if e != nil {
		return nil, true, e
	}
	return transferAttributes(after), true, nil
}
