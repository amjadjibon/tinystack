package rekognition

import (
	"context"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/rekognition"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindCollection = "collection"
	modelVersion   = "1.0"
)

type collection struct {
	Tags    map[string]string
	ID      string
	Created float64
}

func collectionARN(scope request.Scope, id string) string {
	return "arn:aws:rekognition:" + scope.Region + ":" + scope.AccountID + ":collection/" + id
}

func readCollection(tx storage.ReadTx, key string) (*collection, error) {
	row, ok, err := storage.Read[collection](tx, key)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, awserror.New("ResourceNotFoundException", "collection does not exist")
	}

	return row, nil
}

func (s *Service) CreateCollection(ctx context.Context, in *api.CreateCollectionRequest) (
	*api.CreateCollectionResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	if tagErr := checkTags(in.Tags); tagErr != nil {
		return nil, tagErr
	}

	id := common.Value(in.CollectionId)
	key := scope.Key(serviceName, kindCollection, id)
	row := collection{ID: id, Created: common.Unix(time.Now()), Tags: common.CloneTags(in.Tags)}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); ok {
			return awserror.New("ResourceAlreadyExistsException", "collection already exists")
		}

		return storage.Write(tx, key, row)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateCollectionResponse{
		CollectionArn: new(collectionARN(scope, id)), FaceModelVersion: new(modelVersion), StatusCode: new(int64(200)),
	}, nil
}

func (s *Service) DescribeCollection(ctx context.Context, in *api.DescribeCollectionRequest) (
	*api.DescribeCollectionResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	var row *collection
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		row, e = readCollection(tx, scope.Key(serviceName, kindCollection, common.Value(in.CollectionId)))
		return e
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeCollectionResponse{
		CollectionARN: new(collectionARN(scope, row.ID)), CreationTimestamp: new(row.Created),
		FaceCount: new(int64(0)), UserCount: new(int64(0)), FaceModelVersion: new(modelVersion),
	}, nil
}

func (s *Service) ListCollections(ctx context.Context, in *api.ListCollectionsRequest) (
	*api.ListCollectionsResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit := *common.OrElse(in.MaxResults, new(int64(100)))
	if limit < 1 {
		return nil, invalid("MaxResults must be positive")
	}

	var rows []*collection
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		rows, e = storage.Records[collection](tx, scope.Prefix(serviceName, kindCollection))
		return e
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		rows, func(row *collection) string { return row.ID }, in.NextToken, int(limit),
		awserror.New("InvalidPaginationTokenException", "invalid NextToken"),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListCollectionsResponse{NextToken: next}
	for _, row := range page {
		out.CollectionIds = append(out.CollectionIds, row.ID)
		out.FaceModelVersions = append(out.FaceModelVersions, modelVersion)
	}

	return out, nil
}

func (s *Service) DeleteCollection(ctx context.Context, in *api.DeleteCollectionRequest) (
	*api.DeleteCollectionResponse, error,
) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	key := scope.Key(serviceName, kindCollection, common.Value(in.CollectionId))
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, e := readCollection(tx, key); e != nil {
			return e
		}

		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteCollectionResponse{StatusCode: new(int64(200))}, nil
}
