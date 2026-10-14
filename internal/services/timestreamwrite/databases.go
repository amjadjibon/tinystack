package timestreamwrite

import (
	"context"
	"time"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type databaseRecord struct {
	tagSet
	Created time.Time
	Updated time.Time
	Name    string
	KmsKey  string
}

func (r *databaseRecord) output(scope request.Scope, tables int) *api.Database {
	return &api.Database{
		Arn:             new(databaseARN(scope, r.Name)),
		DatabaseName:    new(r.Name),
		TableCount:      new(int64(tables)),
		KmsKeyId:        new(r.KmsKey),
		CreationTime:    common.Seconds(r.Created),
		LastUpdatedTime: common.Seconds(r.Updated),
	}
}

// tableCount counts the tables of a database.
func tableCount(tx storage.ReadTx, scope request.Scope, db string) int {
	return len(tx.Keys(key(scope, kindTable, db+"/")))
}

// resolveKey returns the KMS key of a database, which is the account's AWS managed key unless one is named.
func resolveKey(scope request.Scope, id *string) (string, error) {
	if id == nil {
		return "arn:aws:kms:" + scope.Region + ":" + scope.AccountID + ":key/" + common.RandomID(8) + "-" +
			common.RandomID(4) + "-" + common.RandomID(4) + "-" + common.RandomID(4) + "-" + common.RandomID(12), nil
	}

	if *id == "" || len(*id) > 2048 {
		return "", invalid("KmsKeyId must be 1 to 2048 characters long.")
	}

	return *id, nil
}

func (s *Service) CreateDatabase(
	ctx context.Context,
	in *api.CreateDatabaseRequest,
) (*api.CreateDatabaseResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, err := checkName("DatabaseName", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	kms, err := resolveKey(scope, in.KmsKeyId)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	r := &databaseRecord{Name: db, KmsKey: kms, Created: now, Updated: now, tagSet: tagSet{Tags: tags}}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(key(scope, kindDatabase, db)); exists {
			return fault("ConflictException", "The database %s already exists.", db)
		}

		return storage.Write(tx, key(scope, kindDatabase, db), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateDatabaseResponse{Database: r.output(scope, 0)}, nil
}

func (s *Service) DescribeDatabase(
	ctx context.Context,
	in *api.DescribeDatabaseRequest,
) (*api.DescribeDatabaseResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, err := checkName("DatabaseName", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	var out *api.Database
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		r, found, e := storage.Read[databaseRecord](tx, key(scope, kindDatabase, db))
		if e != nil {
			return e
		}

		if !found {
			return databaseNotFound(db)
		}

		out = r.output(scope, tableCount(tx, scope, db))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeDatabaseResponse{Database: out}, nil
}

func (s *Service) UpdateDatabase(
	ctx context.Context,
	in *api.UpdateDatabaseRequest,
) (*api.UpdateDatabaseResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, err := checkName("DatabaseName", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	if in.KmsKeyId == nil {
		return nil, invalid("KmsKeyId is required.")
	}

	kms, err := resolveKey(scope, in.KmsKeyId)
	if err != nil {
		return nil, err
	}

	var out *api.Database
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, found, e := storage.Read[databaseRecord](tx, key(scope, kindDatabase, db))
		if e != nil {
			return e
		}

		if !found {
			return databaseNotFound(db)
		}

		r.KmsKey, r.Updated = kms, time.Now().UTC()
		out = r.output(scope, tableCount(tx, scope, db))

		return storage.Write(tx, key(scope, kindDatabase, db), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateDatabaseResponse{Database: out}, nil
}

func (s *Service) DeleteDatabase(
	ctx context.Context,
	in *api.DeleteDatabaseRequest,
) (*api.DeleteDatabaseOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	db, err := checkName("DatabaseName", in.DatabaseName)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, found := tx.Get(key(scope, kindDatabase, db)); !found {
			return databaseNotFound(db)
		}

		if tableCount(tx, scope, db) > 0 {
			return invalid("The database %s still has tables; delete them first.", db)
		}

		tx.Delete(key(scope, kindDatabase, db))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDatabaseOutput{}, nil
}

func (s *Service) ListDatabases(
	ctx context.Context,
	in *api.ListDatabasesRequest,
) (*api.ListDatabasesResponse, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := pageSize(in.MaxResults)
	if err != nil {
		return nil, err
	}

	var items []*databaseRecord
	counts := map[string]int{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var e error
		if items, e = storage.Records[databaseRecord](tx, scope.Prefix(serviceName, kindDatabase)); e != nil {
			return e
		}

		for _, r := range items {
			counts[r.Name] = tableCount(tx, scope, r.Name)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	page, next, err := common.Page(
		items, func(r *databaseRecord) string { return r.Name }, in.NextToken, limit, invalidToken(),
	)
	if err != nil {
		return nil, err
	}

	out := &api.ListDatabasesResponse{NextToken: next, Databases: []api.Database{}}
	for _, r := range page {
		out.Databases = append(out.Databases, *r.output(scope, counts[r.Name]))
	}

	return out, nil
}

// databaseOf reads the database a table belongs to, in the caller's transaction.
func databaseOf(tx storage.ReadTx, scope request.Scope, db string) error {
	if _, found := tx.Get(key(scope, kindDatabase, db)); !found {
		return databaseNotFound(db)
	}

	return nil
}
