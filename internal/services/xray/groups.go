package xray

import (
	"context"
	"fmt"
	"unicode/utf8"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) CreateGroup(
	ctx context.Context,
	in *api.CreateGroupRequest,
) (*api.CreateGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || in.GroupName == nil {
		return nil, invalid("GroupName is required")
	}

	if checkErr := checkGroupName(in.GroupName); checkErr != nil {
		return nil, checkErr
	}

	tags, tagErr := tagMap(in.Tags)
	if tagErr != nil {
		return nil, tagErr
	}

	if checkErr := tagging.CheckSet(tags); checkErr != nil {
		return nil, checkTagLimit(checkErr)
	}

	id := common.RandomID(24)
	arn := groupARN(scope.Region, scope.AccountID, *in.GroupName, id)

	group := &api.Group{
		GroupName:             in.GroupName,
		GroupARN:              new(arn),
		FilterExpression:      in.FilterExpression,
		InsightsConfiguration: in.InsightsConfiguration,
	}

	rec := groupRecord{
		Group: group,
		Tags:  tags,
	}

	key := scope.Key(serviceName, groupKind, *in.GroupName)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[groupRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			return invalid("Group already exists with name: %s", *in.GroupName)
		}

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateGroupResult{
		Group: group,
	}, nil
}

func (s *Service) GetGroup(
	ctx context.Context,
	in *api.GetGroupRequest,
) (*api.GetGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || (in.GroupName == nil && in.GroupARN == nil) {
		return nil, invalid("GroupName or GroupARN is required")
	}

	var out *api.Group
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, _, findErr := findGroup(tx, scope, in.GroupName, in.GroupARN)
		if findErr != nil {
			return findErr
		}

		out = rec.Group
		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	return &api.GetGroupResult{
		Group: out,
	}, nil
}

func (s *Service) GetGroups(
	ctx context.Context,
	in *api.GetGroupsRequest,
) (*api.GetGroupsResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	prefix := scope.Prefix(serviceName, groupKind)
	var list []groupRecord
	if viewErr := s.store.View(ctx, func(tx storage.ReadTx) error {
		records, readErr := storage.Records[groupRecord](tx, prefix)
		if readErr != nil {
			return readErr
		}

		for _, r := range records {
			if r != nil {
				list = append(list, *r)
			}
		}

		return nil
	}); viewErr != nil {
		return nil, viewErr
	}

	var token *string
	if in != nil {
		token = in.NextToken
	}

	page, nextToken, err := common.Page(list, func(g groupRecord) string {
		return *g.Group.GroupName
	}, token, 50, invalid("Invalid NextToken"))
	if err != nil {
		return nil, err
	}

	var results []api.GroupSummary
	for _, item := range page {
		results = append(results, api.GroupSummary{
			GroupName:             item.Group.GroupName,
			GroupARN:              item.Group.GroupARN,
			FilterExpression:      item.Group.FilterExpression,
			InsightsConfiguration: item.Group.InsightsConfiguration,
		})
	}

	return &api.GetGroupsResult{
		Groups:    results,
		NextToken: nextToken,
	}, nil
}

func (s *Service) UpdateGroup(
	ctx context.Context,
	in *api.UpdateGroupRequest,
) (*api.UpdateGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || (in.GroupName == nil && in.GroupARN == nil) {
		return nil, invalid("GroupName or GroupARN is required")
	}

	var updated *api.Group
	if updateErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, key, findErr := findGroup(tx, scope, in.GroupName, in.GroupARN)
		if findErr != nil {
			return findErr
		}

		if in.FilterExpression != nil {
			rec.Group.FilterExpression = in.FilterExpression
		}

		if in.InsightsConfiguration != nil {
			rec.Group.InsightsConfiguration = in.InsightsConfiguration
		}

		updated = rec.Group
		return storage.Write(tx, key, rec)
	}); updateErr != nil {
		return nil, updateErr
	}

	return &api.UpdateGroupResult{
		Group: updated,
	}, nil
}

func (s *Service) DeleteGroup(
	ctx context.Context,
	in *api.DeleteGroupRequest,
) (*api.DeleteGroupResult, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if in == nil || (in.GroupName == nil && in.GroupARN == nil) {
		return nil, invalid("GroupName or GroupARN is required")
	}

	if deleteErr := s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, key, findErr := findGroup(tx, scope, in.GroupName, in.GroupARN)
		if findErr != nil {
			return findErr
		}

		tx.Delete(key)
		return nil
	}); deleteErr != nil {
		return nil, deleteErr
	}

	return &api.DeleteGroupResult{}, nil
}

func findGroup(
	tx storage.ReadTx,
	scope request.Scope,
	name *string,
	arn *string,
) (record *groupRecord, key string, err error) {
	if name != nil && *name != "" {
		key := scope.Key(serviceName, groupKind, *name)
		rec, found, readErr := storage.Read[groupRecord](tx, key)
		if readErr != nil {
			return nil, "", readErr
		}

		if !found || rec == nil {
			return nil, "", notFound("Group with name %s not found", *name)
		}

		return rec, key, nil
	}

	if arn != nil && *arn != "" {
		prefix := scope.Prefix(serviceName, groupKind)
		for _, key := range tx.Keys(prefix) {
			rec, found, readErr := storage.Read[groupRecord](tx, key)
			if readErr != nil {
				return nil, "", readErr
			}

			if found && rec != nil && rec.Group != nil && rec.Group.GroupARN != nil && *rec.Group.GroupARN == *arn {
				return rec, key, nil
			}
		}

		return nil, "", notFound("Group with ARN %s not found", *arn)
	}

	return nil, "", invalid("GroupName or GroupARN must be specified")
}

func checkGroupName(name *string) error {
	if name == nil || *name == "" {
		return invalid("GroupName cannot be empty")
	}

	if utf8.RuneCountInString(*name) > 32 {
		return invalid("GroupName length must not exceed 32 characters")
	}

	return nil
}

func groupARN(region, account, name, id string) string {
	return fmt.Sprintf("arn:aws:xray:%s:%s:group/%s/%s", region, account, name, id)
}
