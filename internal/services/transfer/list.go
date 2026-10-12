package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func page(
	scope request.Scope,
	operation, binding string,
	maxResults *int64,
	token *string,
) (limit int64, prefix string, after *string, err error) {
	limit = 100
	if maxResults != nil {
		limit = *maxResults
	}
	if limit < 1 || limit > 1000 {
		return 0, "", nil, invalid("MaxResults requires 1-1000; local default is 100")
	}
	prefix = fmt.Sprintf(
		"%x:",
		sha256.Sum256([]byte(scope.AccountID+"/"+scope.Region+"/transfer/"+operation+"/"+binding)),
	)
	if token != nil {
		raw, e := base64.RawURLEncoding.DecodeString(*token)
		if e != nil || len(*token) > 6144 || !strings.HasPrefix(string(raw), prefix) || !utf8.Valid(raw) {
			return 0, "", nil, awserror.New(
				"InvalidNextTokenException",
				"Invalid NextToken for this scope, API and resource",
			)
		}
		after = new(string(raw[len(prefix):]))
	}
	return limit, prefix, after, nil
}

func nextToken(prefix, key string) *string {
	return new(base64.RawURLEncoding.EncodeToString([]byte(prefix + key)))
}

func (s *Service) ListServers(ctx context.Context, in *api.ListServersRequest) (*api.ListServersResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ListServers input is required")
	}
	limit, prefix, after, e := page(scope, "ListServers", "", in.MaxResults, in.NextToken)
	if e != nil {
		return nil, e
	}
	if after != nil && !serverPattern.MatchString(*after) {
		return nil, awserror.New("InvalidNextTokenException", "Invalid server cursor")
	}
	out := &api.ListServersResponse{Servers: []api.ListedServer{}}
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		keys := tx.Keys(scope.Prefix(serviceName, serverKind))
		slices.Sort(keys)
		for _, k := range keys {
			r, _, err := storage.Read[Snapshot](tx, k)
			if err != nil {
				return err
			}
			v := r.Server
			if after != nil && r.ServerID <= *after {
				continue
			}
			if int64(len(out.Servers)) == limit {
				out.NextToken = nextToken(prefix, *out.Servers[len(out.Servers)-1].ServerId)
				break
			}
			out.Servers = append(
				out.Servers,
				api.ListedServer{
					Arn:                  v.Arn,
					ServerId:             v.ServerId,
					Domain:               v.Domain,
					EndpointType:         v.EndpointType,
					IdentityProviderType: v.IdentityProviderType,
					UserCount:            new(userCount(tx, scope, r.ServerID)),
				},
			)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(out), nil
}

func (s *Service) ListUsers(ctx context.Context, in *api.ListUsersRequest) (*api.ListUsersResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ListUsers input is required")
	}
	in = common.Clone(in)
	out := &api.ListUsersResponse{ServerId: in.ServerId, Users: []api.ListedUser{}}
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		parent, err := readServer(tx, scope, common.Value(in.ServerId))
		if err != nil {
			return err
		}
		limit, prefix, after, err := page(
			scope,
			"ListUsers",
			parent.ServerID+"/"+parent.Incarnation,
			in.MaxResults,
			in.NextToken,
		)
		if err != nil {
			return err
		}
		if after != nil && !userPattern.MatchString(*after) {
			return awserror.New("InvalidNextTokenException", "Invalid user cursor")
		}
		keys := tx.Keys(scope.Prefix(serviceName, userKind) + parent.ServerID + "/")
		slices.Sort(keys)
		for _, k := range keys {
			r, _, err := storage.Read[Snapshot](tx, k)
			if err != nil {
				return err
			}
			u := r.User
			if after != nil && *u.UserName <= *after {
				continue
			}
			if int64(len(out.Users)) == limit {
				out.NextToken = nextToken(prefix, *out.Users[len(out.Users)-1].UserName)
				break
			}
			out.Users = append(
				out.Users,
				api.ListedUser{
					Arn:               u.Arn,
					UserName:          u.UserName,
					Role:              u.Role,
					HomeDirectory:     u.HomeDirectory,
					HomeDirectoryType: u.HomeDirectoryType,
					SshPublicKeyCount: new(int64(len(u.SshPublicKeys))),
				},
			)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(out), nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("ListTagsForResource input is required")
	}
	in = common.Clone(in)
	out := &api.ListTagsForResourceResponse{Arn: in.Arn, Tags: []api.Tag{}}
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, err := tagRead(tx, scope, common.Value(in.Arn))
		if err != nil {
			return err
		}
		limit, prefix, after, err := page(
			scope,
			"ListTagsForResource",
			*in.Arn+"/"+r.Incarnation,
			in.MaxResults,
			in.NextToken,
		)
		if err != nil {
			return err
		}
		if after != nil && utf8.RuneCountInString(*after) > 128 {
			return awserror.New("InvalidNextTokenException", "Invalid tag cursor")
		}
		for _, t := range tagList(tagMap(tagsOf(r))) {
			if after != nil && *t.Key <= *after {
				continue
			}
			if int64(len(out.Tags)) == limit {
				out.NextToken = nextToken(prefix, *out.Tags[len(out.Tags)-1].Key)
				break
			}
			out.Tags = append(out.Tags, t)
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(out), nil
}
