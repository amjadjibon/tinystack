package lakeformation

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type assignment struct {
	Resource *api.Resource
	Tags     []api.LFTagPair
}

func tagInput(k *string, values []string) error {
	if common.Value(k) == "" || len(common.Value(k)) > 128 {
		return invalid("TagKey must contain 1 to 128 characters")
	}

	if len(values) == 0 || len(values) > 1000 {
		return invalid("TagValues must contain 1 to 1000 values")
	}

	for _, v := range values {
		if v == "" || len(v) > 256 {
			return invalid("Tag values must contain 1 to 256 characters")
		}
	}

	return nil
}

func tagExists(tx storage.ReadTx, scope request.Scope, t api.LFTagPair) error {
	if err := catalogOK(scope, t.CatalogId); err != nil {
		return err
	}

	if validationErr := tagInput(t.TagKey, t.TagValues); validationErr != nil {
		return validationErr
	}

	old, ok, err := storage.Read[api.LFTagPair](tx, key(scope, "lf-tag", common.Value(t.TagKey)))
	if err != nil {
		return err
	}

	if !ok {
		return missing("LF-tag not found")
	}

	for _, v := range t.TagValues {
		if !slices.Contains(old.TagValues, v) {
			return missing("LF-tag value not found")
		}
	}

	return nil
}

func (s *Service) CreateLFTag(ctx context.Context, in *api.CreateLFTagRequest) (*api.CreateLFTagResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	if validationErr := tagInput(in.TagKey, in.TagValues); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "lf-tag", common.Value(in.TagKey))
		if _, ok := tx.Get(k); ok {
			return awserror.New("AlreadyExistsException", "LF-tag exists")
		}

		return storage.Write(
			tx,
			k,
			api.LFTagPair{CatalogId: new(scope.AccountID), TagKey: in.TagKey, TagValues: merge(nil, in.TagValues)},
		)
	})
	return &api.CreateLFTagResponse{}, err
}

func (s *Service) GetLFTag(ctx context.Context, in *api.GetLFTagRequest) (*api.GetLFTagResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	out := &api.GetLFTagResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, ok, e := storage.Read[api.LFTagPair](tx, key(scope, "lf-tag", common.Value(in.TagKey)))
		if e != nil {
			return e
		}

		if !ok {
			return missing("LF-tag not found")
		}

		out.CatalogId = t.CatalogId
		out.TagKey = t.TagKey
		out.TagValues = t.TagValues
		return nil
	})
	return out, err
}

func (s *Service) UpdateLFTag(ctx context.Context, in *api.UpdateLFTagRequest) (*api.UpdateLFTagResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	if len(in.TagValuesToAdd)+len(in.TagValuesToDelete) == 0 {
		return nil, invalid("Specify values to add or delete")
	}

	if validationErr := tagInput(
		in.TagKey,
		append(slices.Clone(in.TagValuesToAdd), in.TagValuesToDelete...),
	); validationErr != nil {
		return nil, validationErr
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "lf-tag", common.Value(in.TagKey))
		t, ok, e := storage.Read[api.LFTagPair](tx, k)
		if e != nil {
			return e
		}

		if !ok {
			return missing("LF-tag not found")
		}

		values := merge(subtract(t.TagValues, in.TagValuesToDelete), in.TagValuesToAdd)
		if validationErr := tagInput(t.TagKey, values); validationErr != nil {
			return validationErr
		}

		removed := subtract(t.TagValues, values)
		if validationErr := cleanTag(tx, scope, common.Value(t.TagKey), removed, false); validationErr != nil {
			return validationErr
		}

		t.TagValues = values
		return storage.Write(tx, k, t)
	})
	return &api.UpdateLFTagResponse{}, err
}

// cleanTag removes deleted values from assignments and LF-tag permission grants atomically.
func cleanTag(tx storage.WriteTx, scope request.Scope, tag string, values []string, deleted bool) error {
	for _, k := range tx.Keys(scope.Prefix("lakeformation", "assignment")) {
		a, _, err := storage.Read[assignment](tx, k)
		if err != nil {
			return err
		}

		for i := range a.Tags {
			if common.Value(a.Tags[i].TagKey) == tag {
				a.Tags[i].TagValues = subtract(a.Tags[i].TagValues, values)
				if deleted {
					a.Tags[i].TagValues = nil
				}
			}
		}

		a.Tags = slices.DeleteFunc(a.Tags, func(t api.LFTagPair) bool { return len(t.TagValues) == 0 })
		if len(a.Tags) == 0 {
			tx.Delete(k)
		} else if err := storage.Write(tx, k, a); err != nil {
			return err
		}
	}

	for _, k := range tx.Keys(scope.Prefix("lakeformation", "permission")) {
		p, _, err := storage.Read[api.PrincipalResourcePermissions](tx, k)
		if err != nil {
			return err
		}

		if t := p.Resource.LFTag; t != nil && common.Value(t.TagKey) == tag &&
			(deleted || len(subtract(t.TagValues, values)) != len(t.TagValues)) {
			tx.Delete(k)
		}
	}

	return nil
}

func (s *Service) DeleteLFTag(ctx context.Context, in *api.DeleteLFTagRequest) (*api.DeleteLFTagResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		k := key(scope, "lf-tag", common.Value(in.TagKey))
		if _, ok := tx.Get(k); !ok {
			return missing("LF-tag not found")
		}

		if validationErr := cleanTag(tx, scope, common.Value(in.TagKey), nil, true); validationErr != nil {
			return validationErr
		}

		tx.Delete(k)
		return nil
	})
	return &api.DeleteLFTagResponse{}, err
}

func (s *Service) ListLFTags(ctx context.Context, in *api.ListLFTagsRequest) (*api.ListLFTagsResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	if in.ResourceShareType != nil && *in.ResourceShareType != valueAll && *in.ResourceShareType != "FOREIGN" {
		return nil, invalid("Invalid ResourceShareType")
	}

	var items []api.LFTagPair
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		records, e := storage.Records[api.LFTagPair](tx, scope.Prefix("lakeformation", "lf-tag"))
		for _, r := range records {
			if common.Value(in.ResourceShareType) != "FOREIGN" {
				items = append(items, *r)
			}
		}

		return e
	})
	if err != nil {
		return nil, err
	}

	items, next, err := page(
		items,
		func(t api.LFTagPair) string { return recordID(common.Value(t.TagKey)) },
		in.NextToken,
		in.MaxResults,
	)
	return &api.ListLFTagsResponse{LFTags: items, NextToken: next}, err
}

func assignmentKey(scope request.Scope, r *api.Resource) string {
	return key(scope, "assignment", canonicalResource(scope, r))
}

func editAssignment(tx storage.WriteTx, scope request.Scope, r *api.Resource, t api.LFTagPair, remove bool) error {
	k := assignmentKey(scope, r)
	a, _, err := storage.Read[assignment](tx, k)
	if err != nil {
		return err
	}

	if a == nil {
		a = &assignment{Resource: canonicalResource(scope, r)}
	}

	index := slices.IndexFunc(
		a.Tags,
		func(v api.LFTagPair) bool { return common.Value(v.TagKey) == common.Value(t.TagKey) },
	)
	if remove {
		if index >= 0 {
			a.Tags[index].TagValues = subtract(a.Tags[index].TagValues, t.TagValues)
			if len(a.Tags[index].TagValues) == 0 {
				a.Tags = slices.Delete(a.Tags, index, index+1)
			}
		}
	} else {
		// Each resource has exactly one assigned value per LF-tag key.
		if len(t.TagValues) != 1 {
			return invalid("Assign exactly one value per LF-tag key")
		}

		t.CatalogId = new(scope.AccountID)
		if index < 0 {
			a.Tags = append(a.Tags, t)
		} else {
			a.Tags[index] = t
		}
	}

	slices.SortFunc(
		a.Tags,
		func(a, b api.LFTagPair) int { return strings.Compare(common.Value(a.TagKey), common.Value(b.TagKey)) },
	)
	if len(a.Tags) == 0 {
		tx.Delete(k)
		return nil
	}

	return storage.Write(tx, k, a)
}

func (s *Service) assign(
	ctx context.Context,
	catalog *string,
	r *api.Resource,
	tags []api.LFTagPair,
	remove bool,
) ([]api.LFTagError, error) {
	scope, err := scopeOf(ctx, catalog)
	if err != nil {
		return nil, err
	}

	if len(tags) == 0 || len(tags) > 50 {
		return nil, invalid("LFTags must contain 1 to 50 entries")
	}

	failures := []api.LFTagError{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		kind, e := resource(tx, scope, r)
		if e != nil {
			return e
		}

		if kind != resourceDatabase && kind != resourceTable {
			return invalid("LF-tags may be assigned to databases, tables, or columns")
		}

		if r.Table != nil && r.Table.TableWildcard != nil {
			return invalid("Cannot assign LF-tags to table wildcard")
		}

		if r.TableWithColumns != nil && r.TableWithColumns.ColumnWildcard != nil {
			return invalid("Column wildcard assignments are not supported")
		}

		for _, t := range tags {
			e = tagExists(tx, scope, t)
			if e == nil {
				if r.TableWithColumns != nil {
					for _, column := range merge(nil, r.TableWithColumns.ColumnNames) {
						single := common.Clone(r)
						single.TableWithColumns.ColumnNames = []string{column}
						if e = editAssignment(tx, scope, single, t, remove); e != nil {
							break
						}
					}
				} else {
					e = editAssignment(tx, scope, r, t, remove)
				}
			}

			if e != nil {
				a := awserror.From(e)
				if a.StatusCode >= 500 {
					return e
				}

				failures = append(
					failures,
					api.LFTagError{
						LFTag: new(t),
						Error: &api.ErrorDetail{ErrorCode: new(a.Code), ErrorMessage: new(a.Message)},
					},
				)
			}
		}

		return nil
	})
	return failures, err
}

func (s *Service) AddLFTagsToResource(
	ctx context.Context,
	in *api.AddLFTagsToResourceRequest,
) (*api.AddLFTagsToResourceResponse, error) {
	f, e := s.assign(ctx, in.CatalogId, in.Resource, in.LFTags, false)
	return &api.AddLFTagsToResourceResponse{Failures: f}, e
}

func (s *Service) RemoveLFTagsFromResource(
	ctx context.Context,
	in *api.RemoveLFTagsFromResourceRequest,
) (*api.RemoveLFTagsFromResourceResponse, error) {
	f, e := s.assign(ctx, in.CatalogId, in.Resource, in.LFTags, true)
	return &api.RemoveLFTagsFromResourceResponse{Failures: f}, e
}

func (s *Service) GetResourceLFTags(
	ctx context.Context,
	in *api.GetResourceLFTagsRequest,
) (*api.GetResourceLFTagsResponse, error) {
	scope, err := scopeOf(ctx, in.CatalogId)
	if err != nil {
		return nil, err
	}

	out := &api.GetResourceLFTagsResponse{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		kind, e := resource(tx, scope, in.Resource)
		if e != nil {
			return e
		}

		if kind != resourceDatabase && kind != resourceTable {
			return invalid("Expected database, table, or columns")
		}

		a, ok, e := storage.Read[assignment](tx, assignmentKey(scope, in.Resource))
		if e != nil {
			return e
		}

		if in.Resource.Database != nil {
			if ok {
				out.LFTagOnDatabase = a.Tags
			}

			return nil
		}

		var db, table string
		if t := in.Resource.Table; t != nil {
			db = strings.ToLower(common.Value(t.DatabaseName))
			table = strings.ToLower(common.Value(t.Name))
		} else {
			db = strings.ToLower(common.Value(in.Resource.TableWithColumns.DatabaseName))
			table = strings.ToLower(common.Value(in.Resource.TableWithColumns.Name))
		}

		if table == "" {
			return invalid("A named table is required")
		}

		tr := &api.Resource{Table: &api.TableResource{DatabaseName: new(db), Name: new(table)}}
		a, ok, e = storage.Read[assignment](tx, assignmentKey(scope, tr))
		if e != nil {
			return e
		}

		if ok {
			out.LFTagsOnTable = a.Tags
		}

		if !common.Value(in.ShowAssignedLFTags) {
			database := &api.Resource{Database: &api.DatabaseResource{Name: new(db)}}
			inherited, exists, e := storage.Read[assignment](tx, assignmentKey(scope, database))
			if e != nil {
				return e
			}

			if exists {
				out.LFTagsOnTable = inheritTags(inherited.Tags, out.LFTagsOnTable)
			}
		}

		for _, k := range tx.Keys(scope.Prefix("lakeformation", "assignment")) {
			col, _, e := storage.Read[assignment](tx, k)
			if e != nil {
				return e
			}

			t := col.Resource.TableWithColumns
			if t != nil && common.Value(t.DatabaseName) == db && common.Value(t.Name) == table {
				if requested := in.Resource.TableWithColumns; requested != nil && len(requested.ColumnNames) > 0 &&
					!slices.Contains(requested.ColumnNames, t.ColumnNames[0]) {
					continue
				}

				if requested := in.Resource.TableWithColumns; requested != nil && requested.ColumnWildcard != nil &&
					slices.Contains(requested.ColumnWildcard.ExcludedColumnNames, t.ColumnNames[0]) {
					continue
				}
				tags := col.Tags
				if !common.Value(in.ShowAssignedLFTags) {
					tags = inheritTags(out.LFTagsOnTable, tags)
				}

				out.LFTagsOnColumns = append(
					out.LFTagsOnColumns,
					api.ColumnLFTag{Name: new(t.ColumnNames[0]), LFTags: tags},
				)
			}
		}

		return nil
	})
	return out, err
}

// inheritTags applies child overrides by LF-tag key.
func inheritTags(parent, child []api.LFTagPair) []api.LFTagPair {
	out := slices.Clone(parent)
	for _, t := range child {
		index := slices.IndexFunc(
			out,
			func(v api.LFTagPair) bool { return common.Value(v.TagKey) == common.Value(t.TagKey) },
		)
		if index >= 0 {
			out[index] = t
		} else {
			out = append(out, t)
		}
	}

	slices.SortFunc(
		out,
		func(a, b api.LFTagPair) int { return strings.Compare(common.Value(a.TagKey), common.Value(b.TagKey)) },
	)
	return out
}
