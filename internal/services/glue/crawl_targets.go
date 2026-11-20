package glue

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	ddb "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/tabular"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	dynamoSampleLimit int64 = 100
	dynamoString            = "string"
	dynamoBigint            = "bigint"
	dynamoDouble            = "double"
	dynamoBinary            = "binary"
	dynamoArray             = "array"
	dynamoStruct            = "struct"
)

// UseDynamoDB connects the crawler to local DynamoDB before the background worker starts.
func (s *Service) UseDynamoDB(tables *dynamodb.Service) { s.dynamo = tables }

func (s *Service) describeDynamoTarget(ctx context.Context, target api.DynamoDBTarget) (*ddb.TableDescription, error) {
	if s.dynamo == nil {
		return nil, invalid("DynamoDB crawler targets are unavailable")
	}

	if common.Value(target.Path) == "" {
		return nil, invalid("DynamoDB target Path must name a table")
	}

	if strings.HasPrefix(*target.Path, "arn:") {
		scope, err := request.RegionalScope(ctx)
		if err != nil {
			return nil, err
		}

		prefix := "arn:aws:dynamodb:" + scope.Region + ":" + scope.AccountID + ":table/"
		n, local := strings.CutPrefix(*target.Path, prefix)
		if !local || strings.Contains(n, "/") {
			return nil, invalid("DynamoDB targets must name a table in the crawler's account and Region")
		}
	}

	out, err := s.dynamo.DescribeTable(ctx, &ddb.DescribeTableInput{TableName: target.Path})
	if err != nil {
		return nil, invalid("Cannot describe DynamoDB target %s: %s", *target.Path, err)
	}

	return out.Table, nil
}

func (s *Service) discoverDynamoTarget(
	ctx context.Context, r *crawlerRecord, target api.DynamoDBTarget,
) (*crawlTable, error) {
	table, err := s.describeDynamoTarget(ctx, target)
	if err != nil {
		return nil, err
	}

	fields := dynamoFields{}
	var start map[string]ddb.AttributeValue
	var count int64
	for {
		out, err := s.dynamo.Scan(ctx, &ddb.ScanInput{
			TableName: table.TableName, Limit: new(dynamoSampleLimit), ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, err
		}

		for _, item := range common.Value(out.Items) {
			fields.observe(item)
		}

		count += int64(len(common.Value(out.Items)))
		start = out.LastEvaluatedKey
		if len(start) == 0 || (target.ScanAll != nil && !*target.ScanAll) {
			break
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	for _, a := range table.AttributeDefinitions {
		k := common.Value(a.AttributeName)
		if _, ok := fields[k]; !ok {
			kind := dynamoString
			switch common.Value(a.AttributeType) {
			case "N":
				kind = dynamoDouble
			case "B":
				kind = dynamoBinary
			}

			fields[k] = &dynamoSchema{kind: kind}
		}
	}

	var columns []api.Column
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		columns = append(columns, api.Column{Name: new(k), Type: new(fields[k].hiveType())})
	}

	n := crawlTableName(common.Value(r.Crawler.TablePrefix), *table.TableName)
	return &crawlTable{input: api.TableInput{
		Name: new(n), TableType: new("EXTERNAL_TABLE"),
		StorageDescriptor: &api.StorageDescriptor{Columns: columns, Location: table.TableArn},
		Parameters: map[string]string{
			"classification": "dynamodb", "typeOfData": "dynamodb", "recordCount": strconv.FormatInt(count, 10),
			"UPDATED_BY_CRAWLER": *r.Crawler.Name,
		},
	}}, nil
}

type dynamoFields map[string]*dynamoSchema

func (fields dynamoFields) observe(item map[string]ddb.AttributeValue) {
	for name, value := range item {
		schema := fields[name]
		if schema == nil {
			schema = &dynamoSchema{}
			fields[name] = schema
		}

		schema.observe(&value)
	}
}

type dynamoSchema struct {
	fields  dynamoFields
	element *dynamoSchema
	kind    string
}

func (s *dynamoSchema) merge(kind string) {
	switch {
	case s.kind == "", s.kind == kind:
		s.kind = kind
	case s.kind == dynamoBigint && kind == dynamoDouble, s.kind == dynamoDouble && kind == dynamoBigint:
		s.kind = dynamoDouble
	default:
		*s = dynamoSchema{kind: dynamoString}
	}
}

func dynamoNumber(n string) string {
	if _, err := strconv.ParseInt(n, 10, 64); err == nil {
		return dynamoBigint
	}

	return dynamoDouble
}

func (s *dynamoSchema) observe(v *ddb.AttributeValue) {
	switch {
	case v.S != nil:
		s.merge(dynamoString)
	case v.N != nil:
		s.merge(dynamoNumber(*v.N))
	case v.BOOL != nil:
		s.merge("boolean")
	case v.B != nil:
		s.merge(dynamoBinary)
	case v.M != nil:
		s.merge(dynamoStruct)
		if s.kind == dynamoStruct {
			if s.fields == nil {
				s.fields = dynamoFields{}
			}

			s.fields.observe(*v.M)
		}
	case v.L != nil || v.SS != nil || v.NS != nil || v.BS != nil:
		s.merge(dynamoArray)
		if s.kind == dynamoArray {
			if s.element == nil {
				s.element = &dynamoSchema{}
			}

			s.element.observeElements(v)
		}
	}
}

func (s *dynamoSchema) observeElements(v *ddb.AttributeValue) {
	switch {
	case v.SS != nil:
		s.merge(dynamoString)
	case v.BS != nil:
		s.merge(dynamoBinary)
	case v.NS != nil:
		for _, n := range *v.NS {
			s.merge(dynamoNumber(n))
		}
	case v.L != nil:
		for i := range *v.L {
			s.observe(&(*v.L)[i])
		}
	}
}

func (s *dynamoSchema) hiveType() string {
	switch s.kind {
	case dynamoArray:
		return "array<" + s.element.hiveType() + ">"
	case dynamoStruct:
		var fields []string
		for _, name := range slices.Sorted(maps.Keys(s.fields)) {
			fields = append(fields, name+":"+s.fields[name].hiveType())
		}

		return "struct<" + strings.Join(fields, ",") + ">"
	case "":
		return dynamoString
	default:
		return s.kind
	}
}

type catalogCrawlTarget struct {
	database, table, location string
}

func catalogCrawlTargets(tx storage.ReadTx, scope request.Scope, targets []api.CatalogTarget) (
	[]catalogCrawlTarget, error,
) {
	var out []catalogCrawlTarget
	for _, target := range targets {
		db, err := name("database", target.DatabaseName)
		if err != nil {
			return nil, err
		}

		if len(target.Tables) == 0 {
			return nil, invalid("A catalog target requires at least one table")
		}

		for _, t := range target.Tables {
			n, err := name("table", new(t))
			if err != nil {
				return nil, err
			}

			table, err := loadTable(tx, scope, db, n)
			if err != nil {
				return nil, err
			}

			location := ""
			if table.StorageDescriptor != nil {
				location = common.Value(table.StorageDescriptor.Location)
			}

			u, err := url.Parse(location)
			if err != nil || u.Scheme != "s3" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
				return nil, invalid("Catalog target %s.%s must have an S3 location", db, n)
			}

			location = strings.TrimRight(location, "/") + "/"
			entry := catalogCrawlTarget{database: db, table: n, location: location}
			if !slices.Contains(out, entry) {
				out = append(out, entry)
			}
		}
	}

	return out, nil
}

func (s *Service) discoverCatalogTargets(
	ctx context.Context, r *crawlerRecord, custom []*tabular.ClassifyOptions, targets []catalogCrawlTarget,
) ([]*crawlTable, error) {
	var tables []*crawlTable
	for _, target := range targets {
		found, err := s.discoverS3Tables(ctx, r, custom, []api.S3Target{{Path: new(target.location)}}, true)
		if err != nil {
			return nil, err
		}

		if len(found) == 0 {
			continue
		}

		t := found[0]
		t.input.Name, t.catalog = new(target.table), new(target)
		tables = append(tables, t)
	}

	return tables, nil
}

func (s *Service) discoverTables(
	ctx context.Context, r *crawlerRecord, custom []*tabular.ClassifyOptions, catalogTargets []catalogCrawlTarget,
) ([]*crawlTable, error) {
	if len(catalogTargets) > 0 {
		return s.discoverCatalogTargets(ctx, r, custom, catalogTargets)
	}

	tables, err := s.discoverS3Tables(ctx, r, custom, r.Crawler.Targets.S3Targets, false)
	if err != nil {
		return nil, err
	}

	for _, target := range r.Crawler.Targets.DynamoDBTargets {
		t, err := s.discoverDynamoTarget(ctx, r, target)
		if err != nil {
			return nil, err
		}

		duplicate := slices.IndexFunc(
			tables,
			func(other *crawlTable) bool { return *other.input.Name == *t.input.Name },
		)
		if duplicate >= 0 {
			if *tables[duplicate].input.StorageDescriptor.Location == *t.input.StorageDescriptor.Location {
				continue
			}

			return nil, invalid("Multiple crawler targets produce table %s", *t.input.Name)
		}

		tables = append(tables, t)
	}

	return tables, nil
}

func (s *Service) applyCatalogTarget(
	tx storage.WriteTx, scope request.Scope, r *crawlerRecord, t *crawlTable, m *api.CrawlerMetrics,
) error {
	db, n := t.catalog.database, t.catalog.table
	table, err := loadTable(tx, scope, db, n)
	if err != nil {
		return err
	}

	if table.StorageDescriptor == nil ||
		strings.TrimRight(common.Value(table.StorageDescriptor.Location), "/")+"/" != t.catalog.location {
		return invalid("Catalog target %s.%s changed location during the crawl", db, n)
	}

	write := common.Value(r.Crawler.SchemaChangePolicy.UpdateBehavior) == updateInDatabase
	if len(table.PartitionKeys) > 0 && !slices.EqualFunc(table.PartitionKeys, t.input.PartitionKeys,
		func(a, b api.Column) bool { return strings.EqualFold(common.Value(a.Name), common.Value(b.Name)) }) {
		return invalid("Discovered partitions do not match the keys of catalog target %s.%s", db, n)
	}

	if !write && len(table.PartitionKeys) != len(t.input.PartitionKeys) {
		return invalid("Catalog target %s.%s requires partition keys or UPDATE_IN_DATABASE", db, n)
	}

	now := common.Unix(s.now())
	if write {
		if err := archiveTable(tx, scope, db, n, table); err != nil {
			return err
		}

		sd, inferred := table.StorageDescriptor, t.input.StorageDescriptor
		sd.Columns = inferred.Columns
		sd.InputFormat = common.OrElse(sd.InputFormat, inferred.InputFormat)
		sd.OutputFormat = common.OrElse(sd.OutputFormat, inferred.OutputFormat)
		sd.SerdeInfo = common.OrElse(sd.SerdeInfo, inferred.SerdeInfo)
		sd.Compressed = common.OrElse(sd.Compressed, inferred.Compressed)
		if len(table.PartitionKeys) == 0 {
			table.PartitionKeys = t.input.PartitionKeys
		}

		if table.Parameters == nil {
			table.Parameters = map[string]string{}
		}

		maps.Copy(table.Parameters, t.input.Parameters)
		version, _ := strconv.Atoi(common.Value(table.VersionId))
		table.UpdateTime, table.VersionId = new(now), new(strconv.Itoa(version+1))
		if err := storage.Write(tx, tableKey(scope, db, n), table); err != nil {
			return err
		}

		*m.TablesUpdated++
	}

	t.input.StorageDescriptor = table.StorageDescriptor
	return s.applyCrawlPartitions(tx, scope, r, t, db, n, write, now)
}
