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

var kmsKeyArnPattern = regexp.MustCompile(`^arn:aws[-a-z]*:kms:[-a-z0-9]+:\d{12}:key/[-a-f0-9]+$`)

type workspaceRecord struct {
	CreatedAt   time.Time         `json:"createdAt"`
	Tags        map[string]string `json:"tags,omitempty"`
	WorkspaceID string            `json:"workspaceId"`
	Alias       string            `json:"alias,omitempty"`
	KmsKeyArn   string            `json:"kmsKeyArn,omitempty"`
}

func (w *workspaceRecord) tagMap() map[string]string        { return w.Tags }
func (w *workspaceRecord) setTagMap(tags map[string]string) { w.Tags = tags }

func validateAlias(alias *string) error {
	if alias != nil && (*alias == "" || len(*alias) > 100) {
		return awserror.Validation("alias must be between 1 and 100 characters.")
	}

	return nil
}

func (s *Service) CreateWorkspace(
	ctx context.Context,
	in *api.CreateWorkspaceRequest,
) (*api.CreateWorkspaceResponse, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope := meta.Scope
	err = validateAlias(in.Alias)
	if err != nil {
		return nil, err
	}

	if in.KmsKeyArn != nil && !kmsKeyArnPattern.MatchString(*in.KmsKeyArn) {
		return nil, awserror.Validation("kmsKeyArn must be a KMS key ARN.")
	}

	var tags map[string]string
	if len(in.Tags) > 0 {
		err = tagging.CheckSet(in.Tags)
		if err != nil {
			return nil, awserror.Validation("%s", err.Error())
		}

		tags = maps.Clone(in.Tags)
	}

	id := newWorkspaceID()
	now := time.Now().UTC()
	record := workspaceRecord{
		WorkspaceID: id,
		Alias:       common.Value(in.Alias),
		KmsKeyArn:   common.Value(in.KmsKeyArn),
		CreatedAt:   now,
		Tags:        tags,
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		return storage.Write(tx, key(scope, kindWorkspace, id), record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateWorkspaceResponse{
		Arn:         new(workspaceArn(scope, id)),
		KmsKeyArn:   optional(record.KmsKeyArn),
		Status:      &api.WorkspaceStatus{StatusCode: new(statusActive)},
		Tags:        maps.Clone(tags),
		WorkspaceId: new(id),
	}, nil
}

func (s *Service) DescribeWorkspace(
	ctx context.Context,
	in *api.DescribeWorkspaceRequest,
) (*api.DescribeWorkspaceResponse, error) {
	meta, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	scope := meta.Scope
	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	var record *workspaceRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[workspaceRecord](tx, key(scope, kindWorkspace, workspaceID))
		if e != nil {
			return e
		}

		if !found {
			return notFound("Workspace", workspaceID)
		}

		record = r
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeWorkspaceResponse{
		Workspace: &api.WorkspaceDescription{
			Alias:              optional(record.Alias),
			Arn:                new(workspaceArn(scope, record.WorkspaceID)),
			CreatedAt:          common.Seconds(record.CreatedAt),
			KmsKeyArn:          optional(record.KmsKeyArn),
			PrometheusEndpoint: new(prometheusEndpoint(&meta, record.WorkspaceID)),
			Status:             &api.WorkspaceStatus{StatusCode: new(statusActive)},
			Tags:               maps.Clone(record.Tags),
			WorkspaceId:        new(record.WorkspaceID),
		},
	}, nil
}

func (s *Service) ListWorkspaces(
	ctx context.Context,
	in *api.ListWorkspacesRequest,
) (*api.ListWorkspacesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var all []*workspaceRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[workspaceRecord](tx, scope.Prefix(serviceName, kindWorkspace))
		if e != nil {
			return e
		}

		for _, r := range records {
			if alias := strings.TrimSpace(common.Value(in.Alias)); !strings.HasPrefix(r.Alias, alias) {
				continue
			}

			all = append(all, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(all, func(r *workspaceRecord) string {
		return r.WorkspaceID
	}, in.NextToken, limit, invalidToken())
	if err != nil {
		return nil, err
	}

	summaries := make([]api.WorkspaceSummary, len(page))
	for i, r := range page {
		summaries[i] = api.WorkspaceSummary{
			Alias:       optional(r.Alias),
			Arn:         new(workspaceArn(scope, r.WorkspaceID)),
			CreatedAt:   common.Seconds(r.CreatedAt),
			KmsKeyArn:   optional(r.KmsKeyArn),
			Status:      &api.WorkspaceStatus{StatusCode: new(statusActive)},
			Tags:        maps.Clone(r.Tags),
			WorkspaceId: new(r.WorkspaceID),
		}
	}

	return &api.ListWorkspacesResponse{
		NextToken:  next,
		Workspaces: summaries,
	}, nil
}

func (s *Service) UpdateWorkspaceAlias(
	ctx context.Context,
	in *api.UpdateWorkspaceAliasRequest,
) (*api.UpdateWorkspaceAliasOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	err = validateAlias(in.Alias)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[workspaceRecord](tx, key(scope, kindWorkspace, workspaceID))
		if e != nil {
			return e
		}

		if !found {
			return notFound("Workspace", workspaceID)
		}

		r.Alias = common.Value(in.Alias)
		return storage.Write(tx, key(scope, kindWorkspace, workspaceID), *r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateWorkspaceAliasOutput{}, nil
}

func (s *Service) DeleteWorkspace(
	ctx context.Context,
	in *api.DeleteWorkspaceRequest,
) (*api.DeleteWorkspaceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	workspaceID := common.Value(in.WorkspaceId)
	if workspaceID == "" {
		return nil, awserror.Validation("workspaceId is required.")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, e := storage.Read[workspaceRecord](tx, key(scope, kindWorkspace, workspaceID))
		if e != nil {
			return e
		}

		if !found {
			return notFound("Workspace", workspaceID)
		}

		tx.Delete(key(scope, kindWorkspace, workspaceID))
		tx.Delete(key(scope, kindAlertManagerDefinition, workspaceID))
		tx.Delete(key(scope, kindLoggingConfiguration, workspaceID))

		prefix := scope.Prefix(serviceName, kindRuleGroupsNamespace) + workspaceID + "/"
		for _, k := range tx.Keys(prefix) {
			tx.Delete(k)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteWorkspaceOutput{}, nil
}
