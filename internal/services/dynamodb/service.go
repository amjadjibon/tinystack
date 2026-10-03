package dynamodb

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	returnAllOld = "ALL_OLD"
	selectCount  = "COUNT"
)

type Service struct {
	store  storage.Store
	logger *slog.Logger
	// caller writes exports to S3; see UseCaller.
	caller *catalog.Caller
	// ctx ends the TTL sweeper on Stop.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
}

// New returns a DynamoDB service and starts its TTL sweeper, which runs until
// Stop.
func New(store storage.Store) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{store: store, logger: slog.Default(), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go s.sweep()
	return s
}

// Stop ends the TTL sweeper and waits for it. Later calls return at once.
func (s *Service) Stop() {
	s.stop.Do(func() {
		s.cancel()
		<-s.done
	})
}

// table is the table record. Each item is its own record under the table's
// key (itemRecord), and the description's item count and size are kept
// current as items are written.
type table struct {
	Description api.TableDescription
	Tags        map[string]string `json:",omitempty"`
	// TTL is the attribute that holds item expiry times while TTL is enabled.
	TTL string `json:",omitempty"`
	// Sequence is the sequence number of the table's last stream record.
	Sequence int64 `json:",omitempty"`
	// PITR is when point-in-time recovery was enabled, in Unix seconds, or
	// zero while it is off; PITRDays is its recovery period.
	PITR     float64 `json:",omitempty"`
	PITRDays int64   `json:",omitempty"`
}

func value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}

	return v
}

var tableName = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`)

func tableKey(ctx context.Context, name *string) (string, error) {
	if name == nil || !tableName.MatchString(*name) {
		return "", awserror.Validation("TableName must be 3–255 characters using letters, digits, _, -, or .")
	}

	m, ok := request.FromContext(ctx)
	if !ok || m.AccountID == "" || m.Region == "" {
		return "", fmt.Errorf("missing request identity")
	}

	return m.AccountID + "/" + m.Region + "/dynamodb/" + *name, nil
}

func load(tx storage.ReadTx, key string) (*table, error) {
	data, ok := tx.Get(key)
	if !ok {
		return nil, awserror.New("ResourceNotFoundException", "Requested resource not found")
	}

	var t table
	if guardErr := json.Unmarshal(data, &t); guardErr != nil {
		return nil, fmt.Errorf("decode table: %w", guardErr)
	}

	return &t, nil
}

func save(tx storage.WriteTx, key string, t *table) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode table: %w", err)
	}

	tx.Put(key, data)
	return nil
}

func (s *Service) CreateTable(ctx context.Context, in *api.CreateTableInput) (*api.CreateTableOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	types := map[string]string{}
	for _, d := range in.AttributeDefinitions {
		name, kind := value(d.AttributeName), value(d.AttributeType)
		if name == "" || len(name) > 255 || (kind != "S" && kind != "N" && kind != "B") || types[name] != "" {
			return nil, awserror.Validation("Invalid or duplicate attribute definition")
		}

		types[name] = kind
	}

	used := map[string]bool{}
	if guardErr := checkKeySchema(in.KeySchema, types, used); guardErr != nil {
		return nil, guardErr
	}

	tags, err := tagMap(in.Tags)
	if err != nil {
		return nil, err
	}

	mode := value(in.BillingMode)
	if mode == "" {
		mode = "PROVISIONED"
	}

	if mode != "PAY_PER_REQUEST" && mode != "PROVISIONED" {
		return nil, awserror.Validation("Invalid BillingMode")
	}

	// throughput describes the capacity of the table or an index in mode.
	throughput := func(p *api.ProvisionedThroughput) (*api.ProvisionedThroughputDescription, error) {
		out := &api.ProvisionedThroughputDescription{
			ReadCapacityUnits:      new(int64(0)),
			WriteCapacityUnits:     new(int64(0)),
			NumberOfDecreasesToday: new(int64(0)),
		}

		if mode == "PAY_PER_REQUEST" {
			if p != nil {
				return nil, awserror.Validation("ProvisionedThroughput is not valid for PAY_PER_REQUEST")
			}

			return out, nil
		}

		if p == nil || value(p.ReadCapacityUnits) < 1 || value(p.WriteCapacityUnits) < 1 {
			return nil, awserror.Validation("Positive provisioned read and write capacity required")
		}

		out.ReadCapacityUnits = p.ReadCapacityUnits
		out.WriteCapacityUnits = p.WriteCapacityUnits
		return out, nil
	}

	tableThroughput, err := throughput(in.ProvisionedThroughput)
	if err != nil {
		return nil, err
	}

	m, _ := request.FromContext(ctx)
	arn := fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", m.Region, m.AccountID, *in.TableName)
	globals, locals, err := secondaryIndexDescriptions(in, types, used, arn, throughput)
	if err != nil {
		return nil, err
	}

	if len(used) != len(types) {
		return nil, awserror.Validation("One or more parameter values were invalid: " +
			"every attribute definition must be used by the key schema of the table or an index")
	}

	now := float64(time.Now().UnixMilli()) / 1000
	t := table{
		Description: api.TableDescription{
			TableName:              in.TableName,
			TableStatus:            new("ACTIVE"),
			TableArn:               &arn,
			TableId:                new(rand.Text()),
			CreationDateTime:       &now,
			KeySchema:              in.KeySchema,
			AttributeDefinitions:   in.AttributeDefinitions,
			BillingModeSummary:     &api.BillingModeSummary{BillingMode: &mode},
			ProvisionedThroughput:  tableThroughput,
			ItemCount:              new(int64(0)),
			TableSizeBytes:         new(int64(0)),
			GlobalSecondaryIndexes: globals,
			LocalSecondaryIndexes:  locals,
		},
		Tags: tags,
	}

	viewType, err := streamViewType(in.StreamSpecification)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); ok {
			return awserror.New("ResourceInUseException", "Table already exists")
		}

		if viewType != "" {
			if openErr := openStream(tx, key, &t, viewType, time.Now()); openErr != nil {
				return openErr
			}
		}

		return save(tx, key, &t)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateTableOutput{TableDescription: &t.Description}, nil
}

func (s *Service) DescribeTable(ctx context.Context, in *api.DescribeTableInput) (*api.DescribeTableOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeTableOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		out.Table = &t.Description
		return nil
	})
	return out, err
}

func (s *Service) DeleteTable(ctx context.Context, in *api.DeleteTableInput) (*api.DeleteTableOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	out := &api.DeleteTableOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		tx.Delete(key)
		for _, record := range itemRecords(tx, key, "") {
			tx.Delete(record)
		}

		tx.Delete(ttlIndexKey(key))
		if closeErr := closeStream(tx, key, t, time.Now()); closeErr != nil {
			return closeErr
		}

		t.Description.TableStatus = new("DELETING")
		out.TableDescription = &t.Description
		return nil
	})
	return out, err
}

// ListTables lists only the resolved account and region, with stable pagination.
func (s *Service) ListTables(ctx context.Context, in *api.ListTablesInput) (*api.ListTablesOutput, error) {
	m, ok := request.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("missing request identity")
	}

	limit := int64(100)
	if in.Limit != nil {
		limit = *in.Limit
	}

	if limit < 1 || limit > 100 {
		return nil, awserror.Validation("Limit must be between 1 and 100")
	}

	prefix := m.AccountID + "/" + m.Region + "/dynamodb/"
	out := &api.ListTablesOutput{TableNames: []string{}}
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, key := range tx.Keys(prefix) {
			name := strings.TrimPrefix(key, prefix)
			if strings.Contains(name, "/") || name <= value(in.ExclusiveStartTableName) {
				continue
			}

			if int64(len(out.TableNames)) == limit {
				out.LastEvaluatedTableName = new(out.TableNames[len(out.TableNames)-1])
				break
			}

			out.TableNames = append(out.TableNames, name)
		}

		return nil
	})
	return out, err
}

var numeric = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$`)

func number(v string) (*big.Rat, error) {
	if len(v) > 200 || !numeric.MatchString(v) {
		return nil, awserror.Validation("Invalid number")
	}
	// Bound exponent before parsing to avoid allocations proportional to user input.
	mantissa := v
	for i, c := range v {
		if c == 'e' || c == 'E' {
			mantissa = v[:i]
			exp := new(big.Int)
			if _, ok := exp.SetString(v[i+1:], 10); !ok || !exp.IsInt64() || exp.Int64() > 200 || exp.Int64() < -200 {
				return nil, awserror.Validation("Number exponent out of range")
			}

			break
		}
	}

	digits := strings.Trim(strings.ReplaceAll(strings.TrimLeft(mantissa, "+-"), ".", ""), "0")
	if len(digits) > 38 {
		return nil, awserror.Validation("Number exceeds 38 significant digits")
	}

	r, ok := new(big.Rat).SetString(v)
	if !ok {
		return nil, awserror.Validation("Invalid number")
	}

	abs := new(big.Rat).Abs(r)
	lower := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(130), nil))
	upper := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(126), nil))
	if r.Sign() != 0 && (abs.Cmp(lower) < 0 || abs.Cmp(upper) >= 0) {
		return nil, awserror.Validation("Number out of range")
	}

	return r, nil
}

//nolint:gocritic // A union is a small fixed set of pointers; validation does not mutate it.
func attributeSize(a api.AttributeValue, depth int) (int, error) {
	if depth > 32 {
		return 0, awserror.Validation("Nested attributes exceed depth 32")
	}

	count := 0
	for _, present := range []bool{
		a.B != nil, a.BOOL != nil, a.BS != nil, a.L != nil, a.M != nil,
		a.N != nil, a.NS != nil, a.NULL != nil, a.S != nil, a.SS != nil,
	} {
		if present {
			count++
		}
	}

	if count != 1 {
		return 0, awserror.Validation("AttributeValue must have exactly one type")
	}

	if a.S != nil {
		return len(*a.S), nil
	}

	if a.B != nil {
		return len(*a.B), nil
	}

	if a.N != nil {
		_, err := number(*a.N)
		return len(*a.N), err
	}

	if a.NULL != nil && !*a.NULL {
		return 0, awserror.Validation("NULL must be true")
	}

	n := 1
	if a.L != nil {
		n = 3
		for _, v := range *a.L {
			size, err := attributeSize(v, depth+1)
			if err != nil {
				return 0, err
			}

			n += size + 1
		}
	}

	if a.M != nil {
		n = 3
		for k, v := range *a.M {
			if k == "" {
				return 0, awserror.Validation("Attribute names must not be empty")
			}

			size, err := attributeSize(v, depth+1)
			if err != nil {
				return 0, err
			}

			n += len(k) + size + 1
		}
	}

	var set []string
	if a.SS != nil {
		set = *a.SS
	}

	if a.NS != nil {
		for _, v := range *a.NS {
			r, err := number(v)
			if err != nil {
				return 0, err
			}

			set = append(set, r.RatString())
		}
	}

	if a.BS != nil {
		for _, v := range *a.BS {
			set = append(set, string(v))
		}
	}

	if a.SS != nil || a.NS != nil || a.BS != nil {
		if len(set) == 0 {
			return 0, awserror.Validation("Sets must not be empty")
		}

		seen := map[string]bool{}
		for _, v := range set {
			if seen[v] {
				return 0, awserror.Validation("Set members must be unique")
			}

			seen[v] = true
			n += len(v)
		}
	}

	return n, nil
}
