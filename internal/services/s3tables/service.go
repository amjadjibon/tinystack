// Package s3tables stores customer table-bucket, namespace and table records.
// It does not allocate a warehouse, generate Iceberg files or execute queries.
package s3tables

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName = "s3tables"
	customer    = "customer"
)

const (
	bucketKind    = "bucket"
	namespaceKind = "namespace"
	tableKind     = "table"
)

var _ api.API = (*Service)(nil)

type Service struct{ store storage.Store }

// Snapshot holds a single live record and private compensation guards.
// Namespace identities are scoped names; no namespace ARN exists.
type Snapshot struct {
	Bucket      *api.GetTableBucketResponse
	Namespace   *api.GetNamespaceResponse
	Table       *api.GetTableResponse
	Tags        map[string]string
	BucketARN   string
	Revision    string
	Incarnation string
}

func New(store storage.Store) *Service { return &Service{store: store} }
func regionalScope(ctx context.Context) (request.Scope, error) {
	scope, e := request.RegionalScope(ctx)
	if e != nil {
		return scope, e
	}
	if strings.HasPrefix(scope.Region, "cn-") || strings.HasPrefix(scope.Region, "us-gov-") ||
		strings.HasPrefix(scope.Region, "us-iso") ||
		strings.HasPrefix(scope.Region, "eu-isoe-") {
		return scope, invalid("Only commercial partition S3 Tables metadata scopes are supported")
	}
	return scope, nil
}

func fresh() string                { return uuid.NewV4().String() }
func invalid(message string) error { return awserror.New("BadRequestException", message) }
func missing() error {
	e := awserror.New("NotFoundException", "The specified S3 Tables record does not exist in this account and Region")
	e.StatusCode = 404
	return e
}

func conflict(message string) error {
	e := awserror.New("ConflictException", message)
	e.StatusCode = 409
	return e
}

func bucketARN(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:s3tables:%s:%s:bucket/%s", scope.Region, scope.AccountID, name)
}

func bucketName(scope request.Scope, arn string) (string, error) {
	n, ok := strings.CutPrefix(arn, bucketARN(scope, ""))
	if !ok || ValidateBucketName(n) != nil {
		return "", invalid("tableBucketARN must be an exact commercial S3 Tables bucket ARN in this account and Region")
	}
	return n, nil
}

func key(scope request.Scope, kind, bucket, suffix string) string {
	if suffix != "" {
		bucket += "/" + suffix
	}
	return scope.Key(serviceName, kind, bucket)
}

func recordKey(scope request.Scope, r *Snapshot) string {
	if r.Bucket != nil {
		return key(scope, bucketKind, *r.Bucket.Name, "")
	}
	b, _ := bucketName(scope, r.BucketARN)
	if r.Namespace != nil {
		return key(scope, namespaceKind, b, r.Namespace.Namespace[0])
	}
	return key(scope, tableKind, b, r.Table.Namespace[0]+"/"+*r.Table.Name)
}

func save(tx storage.WriteTx, scope request.Scope, r *Snapshot) error {
	return storage.Write(tx, recordKey(scope, r), r)
}

func read(tx storage.ReadTx, k string) (*Snapshot, error) {
	r, ok, e := storage.Read[Snapshot](tx, k)
	if e != nil {
		return nil, e
	}
	if !ok {
		return nil, missing()
	}
	return r, nil
}

func readBucket(tx storage.ReadTx, scope request.Scope, arn string) (*Snapshot, error) {
	n, e := bucketName(scope, arn)
	if e != nil {
		return nil, e
	}
	return read(tx, key(scope, bucketKind, n, ""))
}

func readNamespace(tx storage.ReadTx, scope request.Scope, arn, ns string) (*Snapshot, error) {
	if e := ValidateNamespaceName(ns); e != nil {
		return nil, e
	}
	b, e := readBucket(tx, scope, arn)
	if e != nil {
		return nil, e
	}
	return read(tx, key(scope, namespaceKind, *b.Bucket.Name, ns))
}

func readTable(tx storage.ReadTx, scope request.Scope, arn, ns, name string) (*Snapshot, error) {
	if e := ValidateTableName(name); e != nil {
		return nil, e
	}
	if _, e := readNamespace(tx, scope, arn, ns); e != nil {
		return nil, e
	}
	b, _ := bucketName(scope, arn)
	return read(tx, key(scope, tableKind, b, ns+"/"+name))
}

func readARN(tx storage.ReadTx, scope request.Scope, arn string) (*Snapshot, error) {
	b, id, table := strings.Cut(arn, "/table/")
	if !table {
		return readBucket(tx, scope, arn)
	}
	name, e := bucketName(scope, b)
	if e != nil {
		return nil, e
	}
	u, e := uuid.Parse(id)
	if e != nil || u.String() != id {
		return nil, invalid("table ARN requires a canonical table UUID")
	}
	if _, e = readBucket(tx, scope, b); e != nil {
		return nil, e
	}
	for _, k := range tx.Keys(key(scope, tableKind, name, "") + "/") {
		r, e := read(tx, k)
		if e != nil {
			return nil, e
		}
		if *r.Table.TableARN == arn {
			return r, nil
		}
	}
	return nil, missing()
}

func (s *Service) CreateTableBucket(
	ctx context.Context,
	in *api.CreateTableBucketRequest,
) (*api.CreateTableBucketResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	name := common.Value(in.Name)
	tags := common.CloneTags(in.Tags)
	if checkErr := ValidateBucketName(name); checkErr != nil {
		return nil, checkErr
	}
	if checkErr := ValidateTags(tags); checkErr != nil {
		return nil, checkErr
	}
	now := time.Now().UTC()
	arn := bucketARN(scope, name)
	r := &Snapshot{
		Bucket: &api.GetTableBucketResponse{
			Arn:            new(arn),
			Name:           new(name),
			OwnerAccountId: new(scope.AccountID),
			CreatedAt:      &now,
			TableBucketId:  new(fresh()),
			Type:           new(customer),
		},
		Tags:        tags,
		BucketARN:   arn,
		Incarnation: fresh(),
		Revision:    fresh(),
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(recordKey(scope, r)); ok {
			return conflict("A table bucket with this name already exists")
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.CreateTableBucketResponse{Arn: new(arn)}, nil
}

func (s *Service) GetTableBucket(
	ctx context.Context,
	in *api.GetTableBucketRequest,
) (*api.GetTableBucketResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn := common.Value(in.TableBucketARN)
	var r *Snapshot
	e = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var err error; r, err = readBucket(tx, scope, arn); return err },
	)
	if e != nil {
		return nil, e
	}
	return common.Clone(r.Bucket), nil
}

func (s *Service) DeleteTableBucket(
	ctx context.Context,
	in *api.DeleteTableBucketRequest,
) (*api.DeleteTableBucketOutput, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn := common.Value(in.TableBucketARN)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readBucket(tx, scope, arn)
		if err != nil {
			return err
		}
		b := *r.Bucket.Name
		if len(tx.Keys(key(scope, namespaceKind, b, "")+"/")) != 0 ||
			len(tx.Keys(key(scope, tableKind, b, "")+"/")) != 0 {
			return conflict("Delete all namespaces and tables before deleting the table bucket")
		}
		tx.Delete(recordKey(scope, r))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteTableBucketOutput{}, nil
}

func (s *Service) CreateNamespace(
	ctx context.Context,
	in *api.CreateNamespaceRequest,
) (*api.CreateNamespaceResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil || len(in.Namespace) != 1 {
		return nil, invalid("namespace requires exactly one component")
	}
	arn, ns := common.Value(in.TableBucketARN), in.Namespace[0]
	if checkErr := ValidateNamespaceName(ns); checkErr != nil {
		return nil, checkErr
	}
	now := time.Now().UTC()
	r := &Snapshot{
		Namespace: &api.GetNamespaceResponse{
			Namespace:      []string{ns},
			CreatedAt:      &now,
			CreatedBy:      new(scope.AccountID),
			OwnerAccountId: new(scope.AccountID),
			NamespaceId:    new(fresh()),
		},
		BucketARN:   arn,
		Incarnation: fresh(),
		Revision:    fresh(),
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		b, err := readBucket(tx, scope, arn)
		if err != nil {
			return err
		}
		r.Namespace.TableBucketId = new(*b.Bucket.TableBucketId)
		if _, ok := tx.Get(recordKey(scope, r)); ok {
			return conflict("A namespace with this name already exists")
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.CreateNamespaceResponse{TableBucketARN: new(arn), Namespace: []string{ns}}, nil
}

func (s *Service) GetNamespace(ctx context.Context, in *api.GetNamespaceRequest) (*api.GetNamespaceResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn, ns := common.Value(in.TableBucketARN), common.Value(in.Namespace)
	var r *Snapshot
	e = s.store.View(
		ctx,
		func(tx storage.ReadTx) error { var err error; r, err = readNamespace(tx, scope, arn, ns); return err },
	)
	if e != nil {
		return nil, e
	}
	return common.Clone(r.Namespace), nil
}

func (s *Service) DeleteNamespace(
	ctx context.Context,
	in *api.DeleteNamespaceRequest,
) (*api.DeleteNamespaceOutput, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn, ns := common.Value(in.TableBucketARN), common.Value(in.Namespace)
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readNamespace(tx, scope, arn, ns)
		if err != nil {
			return err
		}
		b, _ := bucketName(scope, arn)
		if len(tx.Keys(key(scope, tableKind, b, ns)+"/")) != 0 {
			return conflict("Delete or move all tables before deleting the namespace")
		}
		tx.Delete(recordKey(scope, r))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteNamespaceOutput{}, nil
}

func (s *Service) CreateTable(ctx context.Context, in *api.CreateTableRequest) (*api.CreateTableResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn, ns, name, format := common.Value(
		in.TableBucketARN,
	), common.Value(
		in.Namespace,
	), common.Value(
		in.Name,
	), common.Value(
		in.Format,
	)
	tags := common.CloneTags(in.Tags)
	if checkErr := ValidateTableName(name); checkErr != nil {
		return nil, checkErr
	}
	if format != "ICEBERG" {
		return nil, invalid("format must be ICEBERG")
	}
	if checkErr := ValidateTags(tags); checkErr != nil {
		return nil, checkErr
	}
	now := time.Now().UTC()
	tableARN := arn + "/table/" + fresh()
	token := fresh()
	r := &Snapshot{
		Table: &api.GetTableResponse{
			Name:           new(name),
			Namespace:      []string{ns},
			Type:           new(customer),
			TableARN:       new(tableARN),
			VersionToken:   new(token),
			CreatedAt:      &now,
			ModifiedAt:     &now,
			CreatedBy:      new(scope.AccountID),
			ModifiedBy:     new(scope.AccountID),
			OwnerAccountId: new(scope.AccountID),
			Format:         new(format),
		},
		Tags:        tags,
		BucketARN:   arn,
		Incarnation: fresh(),
		Revision:    fresh(),
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		n, err := readNamespace(tx, scope, arn, ns)
		if err != nil {
			return err
		}
		r.Table.NamespaceId = new(*n.Namespace.NamespaceId)
		r.Table.TableBucketId = new(*n.Namespace.TableBucketId)
		if _, ok := tx.Get(recordKey(scope, r)); ok {
			return conflict("A table with this name already exists in the namespace")
		}
		return save(tx, scope, r)
	})
	if e != nil {
		return nil, e
	}
	return &api.CreateTableResponse{TableARN: new(tableARN), VersionToken: new(token)}, nil
}

func (s *Service) GetTable(ctx context.Context, in *api.GetTableRequest) (*api.GetTableResponse, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn, ns, name := common.Value(in.TableBucketARN), common.Value(in.Namespace), common.Value(in.Name)
	tableARN := common.Value(in.TableArn)
	if in.TableArn != nil && (in.TableBucketARN != nil || in.Namespace != nil || in.Name != nil) {
		return nil, invalid("Specify tableArn or tableBucketARN, namespace and name, exclusively")
	}
	if in.TableArn == nil && (in.TableBucketARN == nil || in.Namespace == nil || in.Name == nil) {
		return nil, invalid("Specify tableArn or all three table identity fields")
	}
	byARN := in.TableArn != nil
	var r *Snapshot
	e = s.store.View(ctx, func(tx storage.ReadTx) error {
		var err error
		if byARN {
			r, err = readARN(tx, scope, tableARN)
			if err == nil && r.Table == nil {
				return invalid("tableArn must identify a table")
			}
		} else {
			r, err = readTable(tx, scope, arn, ns, name)
		}
		return err
	})
	if e != nil {
		return nil, e
	}
	return common.Clone(r.Table), nil
}

func (s *Service) DeleteTable(ctx context.Context, in *api.DeleteTableRequest) (*api.DeleteTableOutput, error) {
	scope, e := regionalScope(ctx)
	if e != nil {
		return nil, e
	}
	if in == nil {
		return nil, invalid("input is required")
	}
	arn, ns, name := common.Value(in.TableBucketARN), common.Value(in.Namespace), common.Value(in.Name)
	token := copyString(in.VersionToken)
	if checkErr := checkToken(token); checkErr != nil {
		return nil, checkErr
	}
	e = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, err := readTable(tx, scope, arn, ns, name)
		if err != nil {
			return err
		}
		if token != nil && *token != *r.Table.VersionToken {
			return conflict("The table versionToken is stale")
		}
		tx.Delete(recordKey(scope, r))
		return nil
	})
	if e != nil {
		return nil, e
	}
	return &api.DeleteTableOutput{}, nil
}

func copyString(p *string) *string {
	if p == nil {
		return nil
	}
	return new(*p)
}

func (s *Service) Resources(ctx context.Context) ([]catalog.Resource, error) {
	out := []catalog.Resource{}
	e := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys("") {
			account, region, _, ok := storage.Scoped(k, serviceName)
			if !ok {
				continue
			}
			r, e := read(tx, k)
			if e != nil {
				return e
			}
			item := catalog.Resource{
				Account: account,
				Region:  region,
				Tags:    maps.Clone(r.Tags),
				Details: map[string]any{"RecordsOnly": true, "ExecutionReady": false, "TableBucketARN": r.BucketARN},
			}
			switch {
			case r.Bucket != nil:
				item.Type = "table-bucket"
				item.Name = *r.Bucket.Name
				item.ARN = *r.Bucket.Arn
				item.Details.(map[string]any)["Configuration"] = common.Clone(r.Bucket)
			case r.Namespace != nil:
				item.Type = namespaceKind
				item.Name = r.BucketARN + "/" + r.Namespace.Namespace[0]
				item.Details.(map[string]any)["Configuration"] = common.Clone(r.Namespace)
			case r.Table != nil:
				item.Type = tableKind
				item.Name = r.Table.Namespace[0] + "/" + *r.Table.Name
				item.ARN = *r.Table.TableARN
				item.Details.(map[string]any)["Configuration"] = common.Clone(r.Table)
			}
			out = append(out, item)
		}
		return nil
	})
	return out, e
}
