package dynamodb

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	returnAllOld = "ALL_OLD"
	selectCount  = "COUNT"
)

type Service struct{ store storage.Store }

func New(store storage.Store) *Service { return &Service{store: store} }

type table struct {
	Description api.TableDescription
	Items       map[string]map[string]api.AttributeValue
}

func ptr[T any](v T) *T { return &v }
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
	t.Description.ItemCount = ptr(int64(len(t.Items)))
	var size int64
	for _, item := range t.Items {
		for name, a := range item {
			n, err := attributeSize(a, 0)
			if err != nil {
				return err
			}

			size += int64(len(name) + n)
		}
	}

	t.Description.TableSizeBytes = &size
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

	if len(in.KeySchema) < 1 || len(in.KeySchema) > 2 || len(in.AttributeDefinitions) != len(in.KeySchema) {
		return nil, awserror.Validation("Define a HASH key and optional RANGE key with matching attribute definitions")
	}

	types := map[string]string{}
	for _, d := range in.AttributeDefinitions {
		name, kind := value(d.AttributeName), value(d.AttributeType)
		if name == "" || len(name) > 255 || (kind != "S" && kind != "N" && kind != "B") || types[name] != "" {
			return nil, awserror.Validation("Invalid or duplicate attribute definition")
		}

		types[name] = kind
	}

	names := map[string]bool{}
	for i, k := range in.KeySchema {
		expected := "HASH"
		if i == 1 {
			expected = "RANGE"
		}

		name := value(k.AttributeName)
		if types[name] == "" || names[name] || value(k.KeyType) != expected {
			return nil, awserror.Validation("Invalid key schema")
		}

		names[name] = true
	}

	mode := value(in.BillingMode)
	if mode == "" {
		mode = "PROVISIONED"
	}

	throughput := &api.ProvisionedThroughputDescription{
		ReadCapacityUnits:      ptr(int64(0)),
		WriteCapacityUnits:     ptr(int64(0)),
		NumberOfDecreasesToday: ptr(int64(0)),
	}

	switch mode {
	case "PAY_PER_REQUEST":
		if in.ProvisionedThroughput != nil {
			return nil, awserror.Validation("ProvisionedThroughput is not valid for PAY_PER_REQUEST")
		}
	case "PROVISIONED":
		if in.ProvisionedThroughput == nil || value(in.ProvisionedThroughput.ReadCapacityUnits) < 1 ||
			value(in.ProvisionedThroughput.WriteCapacityUnits) < 1 {
			return nil, awserror.Validation("Positive provisioned read and write capacity required")
		}

		throughput.ReadCapacityUnits = in.ProvisionedThroughput.ReadCapacityUnits
		throughput.WriteCapacityUnits = in.ProvisionedThroughput.WriteCapacityUnits
	default:
		return nil, awserror.Validation("Invalid BillingMode")
	}

	m, _ := request.FromContext(ctx)
	now := float64(time.Now().UnixMilli()) / 1000
	t := table{
		Description: api.TableDescription{
			TableName:   in.TableName,
			TableStatus: ptr("ACTIVE"),
			TableArn: ptr(
				fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", m.Region, m.AccountID, *in.TableName),
			),
			TableId:               ptr(rand.Text()),
			CreationDateTime:      &now,
			KeySchema:             in.KeySchema,
			AttributeDefinitions:  in.AttributeDefinitions,
			BillingModeSummary:    &api.BillingModeSummary{BillingMode: &mode},
			ProvisionedThroughput: throughput,
		},
		Items: map[string]map[string]api.AttributeValue{},
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, ok := tx.Get(key); ok {
			return awserror.New("ResourceInUseException", "Table already exists")
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
		t.Description.TableStatus = ptr("DELETING")
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
		keys := tx.Keys()
		sort.Strings(keys)
		for _, key := range keys {
			if !strings.HasPrefix(key, prefix) {
				continue
			}

			name := strings.TrimPrefix(key, prefix)
			if name <= value(in.ExclusiveStartTableName) {
				continue
			}

			if int64(len(out.TableNames)) == limit {
				out.LastEvaluatedTableName = ptr(out.TableNames[len(out.TableNames)-1])
				break
			}

			out.TableNames = append(out.TableNames, name)
		}

		return nil
	})
	return out, err
}

func itemKey(t *table, item map[string]api.AttributeValue, exact bool) (string, error) {
	if exact && len(item) != len(t.Description.KeySchema) {
		return "", awserror.Validation("Key must contain exactly the table key attributes")
	}

	parts := []string{}
	for _, k := range t.Description.KeySchema {
		name := value(k.AttributeName)
		a, ok := item[name]
		if !ok {
			return "", awserror.Validation("Missing key attribute %s", name)
		}

		if _, err := attributeSize(a, 0); err != nil {
			return "", err
		}

		kind := ""
		for _, d := range t.Description.AttributeDefinitions {
			if value(d.AttributeName) == name {
				kind = value(d.AttributeType)
			}
		}

		var v string
		switch kind {
		case "S":
			if a.S == nil {
				return "", awserror.Validation("Key type mismatch for %s", name)
			}

			v = *a.S
		case "N":
			if a.N == nil {
				return "", awserror.Validation("Key type mismatch for %s", name)
			}

			r, err := number(*a.N)
			if err != nil {
				return "", err
			}

			v = r.RatString()
		case "B":
			if a.B == nil {
				return "", awserror.Validation("Key type mismatch for %s", name)
			}

			v = string(*a.B)
		}

		maxLength := 2048
		if value(k.KeyType) == "RANGE" {
			maxLength = 1024
		}

		if v == "" || len(v) > maxLength {
			return "", awserror.Validation("Invalid key length for %s", name)
		}

		parts = append(parts, base64.RawStdEncoding.EncodeToString([]byte(v)))
	}

	return strings.Join(parts, ":"), nil
}

func returnValues(p *string) error {
	if p != nil && *p != "NONE" && *p != returnAllOld {
		return awserror.Validation("ReturnValues must be NONE or ALL_OLD")
	}

	return nil
}

func (s *Service) PutItem(ctx context.Context, in *api.PutItemInput) (*api.PutItemOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	if guardErr := returnValues(in.ReturnValues); guardErr != nil {
		return nil, guardErr
	}

	size := 0
	for name, a := range in.Item {
		if name == "" {
			return nil, awserror.Validation("Attribute names must not be empty")
		}

		n, e := attributeSize(a, 0)
		if e != nil {
			return nil, e
		}

		size += len(name) + n
	}

	if size > 400*1024 {
		return nil, awserror.Validation("Item exceeds 400 KB")
	}

	out := &api.PutItemOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Item, false)
		if e != nil {
			return e
		}

		if value(in.ReturnValues) == returnAllOld {
			out.Attributes = t.Items[id]
		}

		t.Items[id] = in.Item
		return save(tx, key, t)
	})
	return out, err
}

// GetItem returns the item, or an empty output when the key is absent.
func (s *Service) GetItem(ctx context.Context, in *api.GetItemInput) (*api.GetItemOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	out := &api.GetItemOutput{}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Key, true)
		if e != nil {
			return e
		}

		out.Item = t.Items[id]
		return nil
	})
	return out, err
}

func (s *Service) DeleteItem(ctx context.Context, in *api.DeleteItemInput) (*api.DeleteItemOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	if guardErr := returnValues(in.ReturnValues); guardErr != nil {
		return nil, guardErr
	}

	out := &api.DeleteItemOutput{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		id, e := itemKey(t, in.Key, true)
		if e != nil {
			return e
		}

		if value(in.ReturnValues) == returnAllOld {
			out.Attributes = t.Items[id]
		}

		delete(t.Items, id)
		return save(tx, key, t)
	})
	return out, err
}

// Scan returns a deterministic page; ordering is a local implementation detail.
func (s *Service) Scan(ctx context.Context, in *api.ScanInput) (*api.ScanOutput, error) {
	key, err := tableKey(ctx, in.TableName)
	if err != nil {
		return nil, err
	}

	if in.Limit != nil && *in.Limit < 1 {
		return nil, awserror.Validation("Limit must be positive")
	}

	selection := value(in.Select)
	if selection != "" && selection != "ALL_ATTRIBUTES" && selection != selectCount {
		return nil, awserror.Validation("Select supports ALL_ATTRIBUTES or COUNT")
	}

	out := &api.ScanOutput{
		Items:        ptr([]map[string]api.AttributeValue{}),
		Count:        ptr(int64(0)),
		ScannedCount: ptr(int64(0)),
	}

	if selection == selectCount {
		out.Items = nil
	}

	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		t, e := load(tx, key)
		if e != nil {
			return e
		}

		after := ""
		if len(in.ExclusiveStartKey) > 0 {
			after, e = itemKey(t, in.ExclusiveStartKey, true)
			if e != nil {
				return e
			}
		}

		ids := make([]string, 0, len(t.Items))
		for id := range t.Items {
			if id > after {
				ids = append(ids, id)
			}
		}

		sort.Strings(ids)
		size := 0
		for i, id := range ids {
			item := t.Items[id]
			itemBytes := 0
			for name, a := range item {
				n, e := attributeSize(a, 0)
				if e != nil {
					return e
				}

				itemBytes += len(name) + n
			}

			if i > 0 && size+itemBytes > 1<<20 {
				out.LastEvaluatedKey = primaryKey(t, t.Items[ids[i-1]])
				break
			}

			size += itemBytes
			if selection != selectCount {
				*out.Items = append(*out.Items, item)
			}

			*out.Count++
			*out.ScannedCount++
			if i < len(ids)-1 && ((in.Limit != nil && *out.Count >= *in.Limit) || size >= 1<<20) {
				out.LastEvaluatedKey = primaryKey(t, item)
				break
			}
		}

		return nil
	})
	return out, err
}

func primaryKey(t *table, item map[string]api.AttributeValue) map[string]api.AttributeValue {
	key := map[string]api.AttributeValue{}
	for _, field := range t.Description.KeySchema {
		name := value(field.AttributeName)
		key[name] = item[name]
	}

	return key
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
