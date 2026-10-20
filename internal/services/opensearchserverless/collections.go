package opensearchserverless

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type collection struct {
	Detail api.CollectionDetail
	Tags   map[string]string
}

func collectionKey(scope request.Scope, id string) string {
	return scope.Key(serviceName, collectionKind, id)
}

func readCollection(tx storage.ReadTx, scope request.Scope, id *string) (*collection, error) {
	r, found, err := storage.Read[collection](tx, collectionKey(scope, common.Value(id)))
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, missing()
	}

	return r, nil
}

func tagMap(tags []api.Tag) (map[string]string, error) {
	out, err := tagging.Map(
		tags,
		func(t api.Tag) (*string, *string) { return t.Key, t.Value },
		awserror.Validation(
			"Tag key and value are required.",
		),
		func(string) error { return awserror.Validation("Duplicate tag.") },
	)
	if err != nil {
		return nil, err
	}

	if err := tagging.CheckSet(out); err != nil {
		return nil, awserror.Validation("%s", err)
	}

	return out, nil
}

func (s *Service) CreateCollection(
	ctx context.Context,
	in *api.CreateCollectionRequest,
) (*api.CreateCollectionResponse, error) {
	if !collectionNamePattern.MatchString(common.Value(in.Name)) {
		return nil, awserror.Validation("Invalid collection name.")
	}

	kind := common.Value(common.OrElse(in.Type, new("SEARCH")))
	standby := common.Value(common.OrElse(in.StandbyReplicas, new("ENABLED")))
	if !slices.Contains([]string{"SEARCH", "TIMESERIES", "VECTORSEARCH"}, kind) ||
		!slices.Contains([]string{"ENABLED", "DISABLED"}, standby) || len(common.Value(in.Description)) > 1000 {
		return nil, awserror.Validation("Invalid collection type, standbyReplicas or description.")
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	return transact(
		ctx,
		s,
		"CreateCollection",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.CreateCollectionResponse, error) {
			rows, err := storage.Records[collection](tx, scope.Prefix(serviceName, collectionKind))
			if err != nil {
				return nil, err
			}

			for _, r := range rows {
				if common.Value(r.Detail.Name) == *in.Name {
					return nil, conflict()
				}
			}

			policies, err := storage.Records[policy](tx, scope.Prefix(serviceName, "security")+"encryption/")
			if err != nil {
				return nil, err
			}

			var matched *policy
			best := -1
			for _, p := range policies {
				if score := policySpecificity(p, "collection/"+*in.Name); score > best {
					matched = p
					best = score
				}
			}

			if matched == nil {
				return nil, awserror.Validation(
					"No matching security policy of encryption type found for collection name: %s.",
					*in.Name,
				)
			}

			id := strings.ToLower(common.RandomID(26))
			now := time.Now().UnixMilli()
			arn := fmt.Sprintf("arn:aws:aoss:%s:%s:collection/%s", scope.Region, scope.AccountID, id)
			endpoint := fmt.Sprintf("https://%s.%s.aoss.amazonaws.com", id, scope.Region)
			key := "auto"
			if doc, ok := matched.Policy.(map[string]any); ok {
				if kms, ok := doc["KmsARN"].(string); ok {
					key = kms
				}
			}

			detail := api.CollectionDetail{
				Id:                 new(id),
				Name:               in.Name,
				Type:               new(kind),
				Status:             new("CREATING"),
				StandbyReplicas:    new(standby),
				Description:        in.Description,
				Arn:                new(arn),
				KmsKeyArn:          new(key),
				CreatedDate:        new(now),
				LastModifiedDate:   new(now),
				CollectionEndpoint: new(endpoint),
				DashboardEndpoint:  new(endpoint + "/_dashboards"),
			}

			r := collection{Detail: detail, Tags: tags}
			return &api.CreateCollectionResponse{CreateCollectionDetail: createCollectionDetail(&detail)},
				storage.Write(tx, collectionKey(scope, id), r)
		},
	)
}

// Reads expose the current lifecycle state, then advance the persisted record for the next observation.
func advanceCollection(tx storage.WriteTx, scope request.Scope, r *collection) error {
	key := collectionKey(scope, common.Value(r.Detail.Id))
	switch common.Value(r.Detail.Status) {
	case "DELETING":
		tx.Delete(key)
	case "CREATING":
		next := common.Clone(r)
		next.Detail.Status = new("ACTIVE")
		next.Detail.LastModifiedDate = new(time.Now().UnixMilli())
		return storage.Write(tx, key, next)
	}

	return nil
}

func (s *Service) BatchGetCollection(
	ctx context.Context,
	in *api.BatchGetCollectionRequest,
) (*api.BatchGetCollectionResponse, error) {
	if (len(in.Ids) == 0) == (len(in.Names) == 0) || len(in.Ids) > 100 || len(in.Names) > 100 {
		return nil, awserror.Validation("Provide either 1 to 100 ids or names.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	out := &api.BatchGetCollectionResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rows, readErr := storage.Records[collection](tx, scope.Prefix(serviceName, collectionKind))
		if readErr != nil {
			return readErr
		}

		values := in.Ids
		if len(in.Names) > 0 {
			values = in.Names
		}

		for _, value := range values {
			found := false
			for _, r := range rows {
				target := common.Value(r.Detail.Id)
				if len(in.Names) > 0 {
					target = common.Value(r.Detail.Name)
				}

				if value != target {
					continue
				}

				found = true
				out.CollectionDetails = append(out.CollectionDetails, r.Detail)
				if advanceErr := advanceCollection(tx, scope, r); advanceErr != nil {
					return advanceErr
				}
			}

			if !found {
				detail := api.CollectionErrorDetail{
					ErrorCode:    new("ResourceNotFoundException"),
					ErrorMessage: new("Collection not found."),
				}

				if len(in.Names) > 0 {
					detail.Name = new(value)
				} else {
					detail.Id = new(value)
				}

				out.CollectionErrorDetails = append(out.CollectionErrorDetails, detail)
			}
		}

		return nil
	})
	return out, err
}

func pageLimit(limit *int64) (int, error) {
	if limit == nil {
		return 20, nil
	}

	if *limit < 1 || *limit > 100 {
		return 0, awserror.Validation("maxResults must be between 1 and 100.")
	}

	return int(*limit), nil
}

func (s *Service) ListCollections(
	ctx context.Context,
	in *api.ListCollectionsRequest,
) (*api.ListCollectionsResponse, error) {
	limit, err := pageLimit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	if f := in.CollectionFilters; f != nil && f.Status != nil &&
		!slices.Contains([]string{"CREATING", "ACTIVE", "DELETING", "FAILED", "UPDATING"}, *f.Status) {
		return nil, awserror.Validation("Invalid collection status.")
	}

	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	rows := []api.CollectionSummary{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		records, e := storage.Records[collection](tx, scope.Prefix(serviceName, collectionKind))
		if e != nil {
			return e
		}

		for _, r := range records {
			d := r.Detail
			f := in.CollectionFilters
			if f != nil && ((f.Name != nil && !strings.Contains(common.Value(d.Name), *f.Name)) ||
				(f.Status != nil && common.Value(d.Status) != *f.Status)) {
				continue
			}

			rows = append(rows, *collectionSummary(&d))
			if advanceErr := advanceCollection(tx, scope, r); advanceErr != nil {
				return advanceErr
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(rows, func(r api.CollectionSummary) string { return common.Value(r.Id) },
		in.NextToken, limit, awserror.Validation("Invalid nextToken."))
	return &api.ListCollectionsResponse{CollectionSummaries: page, NextToken: next}, err
}

func (s *Service) UpdateCollection(
	ctx context.Context,
	in *api.UpdateCollectionRequest,
) (*api.UpdateCollectionResponse, error) {
	if len(common.Value(in.Description)) > 1000 {
		return nil, awserror.Validation("Description exceeds 1000 characters.")
	}

	return transact(
		ctx,
		s,
		"UpdateCollection",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.UpdateCollectionResponse, error) {
			r, err := readCollection(tx, scope, in.Id)
			if err != nil {
				return nil, err
			}

			if common.Value(r.Detail.Status) != "ACTIVE" {
				return nil, conflict()
			}

			if in.Description != nil {
				r.Detail.Description = in.Description
			}

			r.Detail.LastModifiedDate = new(time.Now().UnixMilli())
			return &api.UpdateCollectionResponse{UpdateCollectionDetail: updateCollectionDetail(&r.Detail)},
				storage.Write(tx, collectionKey(scope, common.Value(in.Id)), r)
		},
	)
}

func (s *Service) DeleteCollection(
	ctx context.Context,
	in *api.DeleteCollectionRequest,
) (*api.DeleteCollectionResponse, error) {
	return transact(
		ctx,
		s,
		"DeleteCollection",
		in.ClientToken,
		in,
		func(tx storage.WriteTx, scope request.Scope) (*api.DeleteCollectionResponse, error) {
			r, err := readCollection(tx, scope, in.Id)
			if err != nil {
				return nil, err
			}

			r.Detail.Status = new("DELETING")
			return &api.DeleteCollectionResponse{DeleteCollectionDetail: deleteCollectionDetail(&r.Detail)},
				storage.Write(tx, collectionKey(scope, common.Value(in.Id)), r)
		},
	)
}
