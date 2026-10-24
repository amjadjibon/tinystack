package glue

import (
	"context"
	"maps"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// database is a database's record.
type database struct {
	Tags     map[string]string
	Database api.Database
}

func loadDatabase(tx storage.ReadTx, scope request.Scope, n string) (*database, error) {
	d, ok, err := storage.Read[database](tx, databaseKey(scope, n))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Database %s not found.", n))
	}

	return d, nil
}

func checkTags(tags map[string]string) error {
	if err := tagging.CheckSet(tags); err != nil {
		return invalid("%s", err.Error())
	}

	return nil
}

func (s *Service) CreateDatabase(
	ctx context.Context,
	in *api.CreateDatabaseRequest,
) (*api.CreateDatabaseResponse, error) {
	input := common.OrElse(in.DatabaseInput, &api.DatabaseInput{})
	n, err := name("database", input.Name)
	if err != nil {
		return nil, err
	}

	if checkErr := checkTags(in.Tags); checkErr != nil {
		return nil, checkErr
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, found, readErr := storage.Read[database](tx, databaseKey(scope, n)); readErr != nil || found {
			return common.ErrorsOr(readErr, alreadyExists("Database already exists."))
		}

		return storage.Write(tx, databaseKey(scope, n), &database{
			Tags: common.CloneTags(in.Tags), Database: databaseOf(scope, n, input, common.Unix(s.now())),
		})
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDatabaseResponse{}, nil
}

func databaseOf(scope request.Scope, n string, in *api.DatabaseInput, created float64) api.Database {
	return api.Database{
		Name: new(n), Description: in.Description, LocationUri: in.LocationUri, Parameters: in.Parameters,
		CreateTime: new(created), CreateTableDefaultPermissions: in.CreateTableDefaultPermissions,
		CatalogId: new(scope.AccountID),
	}
}

func (s *Service) GetDatabase(ctx context.Context, in *api.GetDatabaseRequest) (*api.GetDatabaseResponse, error) {
	n, err := name("database", in.Name)
	if err != nil {
		return nil, err
	}

	out := &api.GetDatabaseResponse{}
	err = s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		d, loadErr := loadDatabase(tx, scope, n)
		if loadErr == nil {
			out.Database = &d.Database
		}

		return loadErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetDatabases(ctx context.Context, in *api.GetDatabasesRequest) (*api.GetDatabasesResponse, error) {
	var all []*database
	err := s.view(ctx, in.CatalogId, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		all, err = storage.Records[database](tx, scope.Prefix(service, kindDatabase))
		return err
	})
	if err != nil {
		return nil, err
	}

	selected, next, err := page(all, func(d *database) string { return *d.Database.Name }, in.NextToken,
		in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.GetDatabasesResponse{DatabaseList: []api.Database{}, NextToken: next}
	for _, d := range selected {
		out.DatabaseList = append(out.DatabaseList, d.Database)
	}

	return out, nil
}

func (s *Service) UpdateDatabase(
	ctx context.Context,
	in *api.UpdateDatabaseRequest,
) (*api.UpdateDatabaseResponse, error) {
	n, err := name("database", in.Name)
	if err != nil {
		return nil, err
	}

	input := common.OrElse(in.DatabaseInput, &api.DatabaseInput{})
	if renamed, _ := name("database", input.Name); renamed != n {
		return nil, invalid("TinyStack does not rename databases.")
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		d, loadErr := loadDatabase(tx, scope, n)
		if loadErr != nil {
			return loadErr
		}

		d.Database = databaseOf(scope, n, input, common.Value(d.Database.CreateTime))
		return storage.Write(tx, databaseKey(scope, n), d)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDatabaseResponse{}, nil
}

func (s *Service) DeleteDatabase(
	ctx context.Context,
	in *api.DeleteDatabaseRequest,
) (*api.DeleteDatabaseResponse, error) {
	n, err := name("database", in.Name)
	if err != nil {
		return nil, err
	}

	err = s.update(ctx, in.CatalogId, func(tx storage.WriteTx, scope request.Scope) error {
		if _, loadErr := loadDatabase(tx, scope, n); loadErr != nil {
			return loadErr
		}

		for _, key := range tx.Keys(tableKey(scope, n, "")) {
			deleteTable(tx, scope, n, strings.TrimPrefix(key, tableKey(scope, n, "")))
		}

		tx.Delete(databaseKey(scope, n))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDatabaseResponse{}, nil
}

// tagged resolves a taggable resource in the caller's account and Region.
func tagged(scope request.Scope, arn *string) (kind, n string, err error) {
	prefix := "arn:aws:glue:" + scope.Region + ":" + scope.AccountID + ":"
	rest, ok := strings.CutPrefix(common.Value(arn), prefix)
	if !ok {
		return "", "", invalid("Invalid Glue resource ARN")
	}

	kind, n, ok = strings.Cut(rest, "/")
	if !ok || n == "" || (kind != kindDatabase && kind != kindJob && kind != kindTrigger && kind != kindWorkflow) {
		return "", "", invalid("Unsupported Glue resource ARN")
	}

	if kind == kindDatabase {
		n = strings.ToLower(n)
	}

	return kind, n, nil
}

func loadTagged(
	tx storage.ReadTx,
	scope request.Scope,
	kind, n string,
) (record any, tags map[string]string, err error) {
	switch kind {
	case kindJob:
		r, err := loadJob(tx, scope, n)
		if err != nil {
			return nil, nil, err
		}

		return r, r.Tags, nil
	case kindTrigger:
		r, err := loadTrigger(tx, scope, n)
		if err != nil {
			return nil, nil, err
		}

		return r, r.Tags, nil
	case kindWorkflow:
		r, err := loadWorkflow(tx, scope, n)
		if err != nil {
			return nil, nil, err
		}

		return r, r.Tags, nil
	default:
		r, err := loadDatabase(tx, scope, n)
		if err != nil {
			return nil, nil, err
		}

		return r, r.Tags, nil
	}
}

func (s *Service) retag(ctx context.Context, arn *string, fn func(map[string]string) map[string]string) error {
	return s.update(ctx, nil, func(tx storage.WriteTx, scope request.Scope) error {
		kind, n, err := tagged(scope, arn)
		if err != nil {
			return err
		}

		r, tags, err := loadTagged(tx, scope, kind, n)
		if err != nil {
			return err
		}

		tags = fn(common.CloneTags(tags))
		switch record := r.(type) {
		case *jobRecord:
			record.Tags = tags
		case *triggerRecord:
			record.Tags = tags
		case *workflowRecord:
			record.Tags = tags
		case *database:
			record.Tags = tags
		}

		if err := checkTags(tags); err != nil {
			return err
		}

		key := databaseKey(scope, n)
		if kind != kindDatabase {
			key = orchestrationKey(scope, kind, n)
		}

		return storage.Write(tx, key, r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		if tags == nil {
			tags = map[string]string{}
		}

		maps.Copy(tags, in.TagsToAdd)
		return tags
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.retag(ctx, in.ResourceArn, func(tags map[string]string) map[string]string {
		return tagging.Remove(tags, in.TagsToRemove)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) GetTags(ctx context.Context, in *api.GetTagsRequest) (*api.GetTagsResponse, error) {
	out := &api.GetTagsResponse{Tags: map[string]string{}}
	err := s.view(ctx, nil, func(tx storage.ReadTx, scope request.Scope) error {
		kind, n, err := tagged(scope, in.ResourceArn)
		if err != nil {
			return err
		}

		_, tags, err := loadTagged(tx, scope, kind, n)
		if err != nil {
			return err
		}

		if tags != nil {
			out.Tags = tags
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
