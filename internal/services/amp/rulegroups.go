package amp

import (
	"context"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var namePattern = regexp.MustCompile(`^[0-9A-Za-z][-.0-9A-Z_a-z]*$`)

type ruleGroupsNamespaceRecord struct {
	CreatedAt   time.Time         `json:"createdAt"`
	ModifiedAt  time.Time         `json:"modifiedAt"`
	Tags        map[string]string `json:"tags,omitempty"`
	WorkspaceID string            `json:"workspaceId"`
	Name        string            `json:"name"`
	Data        []byte            `json:"data"`
}

func (r *ruleGroupsNamespaceRecord) tagMap() map[string]string        { return r.Tags }
func (r *ruleGroupsNamespaceRecord) setTagMap(tags map[string]string) { r.Tags = tags }

func validateData(data []byte) error {
	if len(data) == 0 {
		return awserror.Validation("data cannot be empty.")
	}

	return nil
}

func (s *Service) CreateRuleGroupsNamespace(
	ctx context.Context,
	in *api.CreateRuleGroupsNamespaceRequest,
) (*api.CreateRuleGroupsNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	name := common.Value(in.Name)
	if len(name) > 128 || !namePattern.MatchString(name) {
		return nil, awserror.Validation("name must be 1 to 128 characters matching %s.", namePattern)
	}

	err = validateData(in.Data)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	if len(in.Tags) > 0 {
		err = tagging.CheckSet(in.Tags)
		if err != nil {
			return nil, awserror.Validation("%s", err.Error())
		}

		tags = maps.Clone(in.Tags)
	}

	now := time.Now().UTC()
	record := ruleGroupsNamespaceRecord{
		WorkspaceID: workspaceID,
		Name:        name,
		Data:        in.Data,
		CreatedAt:   now,
		ModifiedAt:  now,
		Tags:        tags,
	}

	recordKey := key(scope, kindRuleGroupsNamespace, workspaceID+"/"+name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[ruleGroupsNamespaceRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if found {
			return conflict("RuleGroupsNamespace already exists")
		}

		return storage.Write(tx, recordKey, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRuleGroupsNamespaceResponse{
		Arn:    new(ruleGroupsNamespaceArn(scope, workspaceID, name)),
		Name:   new(name),
		Status: &api.RuleGroupsNamespaceStatus{StatusCode: new(statusActive)},
		Tags:   maps.Clone(tags),
	}, nil
}

func (s *Service) DescribeRuleGroupsNamespace(
	ctx context.Context,
	in *api.DescribeRuleGroupsNamespaceRequest,
) (*api.DescribeRuleGroupsNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	name := common.Value(in.Name)
	if name == "" {
		return nil, awserror.Validation("name is required.")
	}

	var record *ruleGroupsNamespaceRecord
	recordKey := key(scope, kindRuleGroupsNamespace, workspaceID+"/"+name)
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[ruleGroupsNamespaceRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("RuleGroupsNamespace", name)
		}

		record = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeRuleGroupsNamespaceResponse{
		RuleGroupsNamespace: &api.RuleGroupsNamespaceDescription{
			Arn:        new(ruleGroupsNamespaceArn(scope, workspaceID, record.Name)),
			CreatedAt:  common.Seconds(record.CreatedAt),
			Data:       record.Data,
			ModifiedAt: common.Seconds(record.ModifiedAt),
			Name:       new(record.Name),
			Status:     &api.RuleGroupsNamespaceStatus{StatusCode: new(statusActive)},
			Tags:       maps.Clone(record.Tags),
		},
	}, nil
}

func (s *Service) ListRuleGroupsNamespaces(
	ctx context.Context,
	in *api.ListRuleGroupsNamespacesRequest,
) (*api.ListRuleGroupsNamespacesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var all []*ruleGroupsNamespaceRecord
	prefix := scope.Prefix(serviceName, kindRuleGroupsNamespace) + workspaceID + "/"
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		records, e := storage.Records[ruleGroupsNamespaceRecord](tx, prefix)
		if e != nil {
			return e
		}

		for _, r := range records {
			if in.Name != nil && *in.Name != "" && !strings.HasPrefix(r.Name, *in.Name) {
				continue
			}

			all = append(all, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(all, func(r *ruleGroupsNamespaceRecord) string {
		return r.Name
	}, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	summaries := make([]api.RuleGroupsNamespaceSummary, len(page))
	for i, r := range page {
		summaries[i] = api.RuleGroupsNamespaceSummary{
			Arn:        new(ruleGroupsNamespaceArn(scope, workspaceID, r.Name)),
			CreatedAt:  common.Seconds(r.CreatedAt),
			ModifiedAt: common.Seconds(r.ModifiedAt),
			Name:       new(r.Name),
			Status:     &api.RuleGroupsNamespaceStatus{StatusCode: new(statusActive)},
			Tags:       maps.Clone(r.Tags),
		}
	}

	return &api.ListRuleGroupsNamespacesResponse{
		NextToken:            next,
		RuleGroupsNamespaces: summaries,
	}, nil
}

func (s *Service) PutRuleGroupsNamespace(
	ctx context.Context,
	in *api.PutRuleGroupsNamespaceRequest,
) (*api.PutRuleGroupsNamespaceResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	name := common.Value(in.Name)
	if name == "" {
		return nil, awserror.Validation("name is required.")
	}

	err = validateData(in.Data)
	if err != nil {
		return nil, err
	}

	recordKey := key(scope, kindRuleGroupsNamespace, workspaceID+"/"+name)
	var updated *ruleGroupsNamespaceRecord
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		r, found, e := storage.Read[ruleGroupsNamespaceRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("RuleGroupsNamespace", name)
		}

		r.Data = in.Data
		r.ModifiedAt = time.Now().UTC()
		updated = r
		return storage.Write(tx, recordKey, *r)
	})
	if err != nil {
		return nil, err
	}

	return &api.PutRuleGroupsNamespaceResponse{
		Arn:    new(ruleGroupsNamespaceArn(scope, workspaceID, name)),
		Name:   new(name),
		Status: &api.RuleGroupsNamespaceStatus{StatusCode: new(statusActive)},
		Tags:   maps.Clone(updated.Tags),
	}, nil
}

func (s *Service) DeleteRuleGroupsNamespace(
	ctx context.Context,
	in *api.DeleteRuleGroupsNamespaceRequest,
) (*api.DeleteRuleGroupsNamespaceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	name := common.Value(in.Name)
	if name == "" {
		return nil, awserror.Validation("name is required.")
	}

	recordKey := key(scope, kindRuleGroupsNamespace, workspaceID+"/"+name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if e := checkWorkspaceExists(tx, scope, workspaceID); e != nil {
			return e
		}

		_, found, e := storage.Read[ruleGroupsNamespaceRecord](tx, recordKey)
		if e != nil {
			return e
		}

		if !found {
			return notFound("RuleGroupsNamespace", name)
		}

		tx.Delete(recordKey)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRuleGroupsNamespaceOutput{}, nil
}
