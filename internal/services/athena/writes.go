package athena

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tabular"
)

type (
	writeContext struct {
		id, output string
		enforced   bool
	}

	writeContextKey struct{}
	schemaOnlyKey   struct{}
)

func queryTokens(tokens []token) []token {
	var b strings.Builder
	for _, t := range tokens {
		if t.separated {
			b.WriteByte(' ')
		}

		b.WriteString(t.text)
	}

	return mustLex(b.String())
}

func isCTAS(t []token) bool {
	p := &parser{t: t}
	if !p.accept("create", "table") {
		return false
	}

	_, _, err := p.qualified(defaultDB)
	return err == nil && (p.peek().is("with") || p.peek().is("as"))
}

func (s *Service) ctas(
	ctx context.Context,
	database string,
	tokens []token,
	params []string,
) (*resultSet, int64, error) {
	p := &parser{t: tokens}
	p.accept("create", "table")
	db, name, err := p.qualified(database)
	if err != nil {
		return nil, 0, err
	}

	if _, getErr := s.catalog.GetDatabase(ctx, &glueapi.GetDatabaseRequest{Name: new(db)}); getErr != nil {
		return nil, 0, getErr
	}

	props := map[string]string{"format": "PARQUET", "write_compression": "GZIP", "field_delimiter": "\x01"}
	var partitions []string
	if p.accept("with") {
		if expectErr := p.expect("("); expectErr != nil {
			return nil, 0, expectErr
		}

		seen := map[string]bool{}
		for !p.accept(")") {
			key, nameErr := p.name()
			if nameErr != nil {
				return nil, 0, nameErr
			}

			if seen[key] {
				return nil, 0, fmt.Errorf("duplicate table property %s", key)
			}

			seen[key] = true
			if expectErr := p.expect("="); expectErr != nil {
				return nil, 0, expectErr
			}

			if key == "partitioned_by" {
				if expectErr := p.expect("array", "["); expectErr != nil {
					return nil, 0, expectErr
				}

				for !p.accept("]") {
					v, strErr := p.str()
					if strErr != nil {
						return nil, 0, strErr
					}

					partitions = append(partitions, strings.ToLower(v))
					if !p.accept(",") {
						if expectErr := p.expect("]"); expectErr != nil {
							return nil, 0, expectErr
						}

						break
					}
				}
			} else {
				switch key {
				case "format",
					"write_compression",
					"external_location",
					"field_delimiter",
					"table_type",
					"parquet_compression":
				default:
					return nil, 0, fmt.Errorf("NOT_SUPPORTED: Table property %s does not exist", key)
				}

				v, strErr := p.str()
				if strErr != nil {
					return nil, 0, strErr
				}

				props[key] = v
			}

			if !p.accept(",") {
				if expectErr := p.expect(")"); expectErr != nil {
					return nil, 0, expectErr
				}

				break
			}
		}

		if seen["write_compression"] && seen["parquet_compression"] {
			return nil, 0, fmt.Errorf("multiple compression properties specified")
		}

		if seen["parquet_compression"] {
			props["write_compression"] = props["parquet_compression"]
		}
	}

	if typ := props["table_type"]; typ != "" && !strings.EqualFold(typ, "HIVE") {
		return nil, 0, fmt.Errorf("NOT_SUPPORTED: table type %s", typ)
	}

	if expectErr := p.expect("as"); expectErr != nil {
		return nil, 0, expectErr
	}

	selectTokens := p.t[p.i:]
	noData := false
	n := len(selectTokens)
	if n >= 3 && selectTokens[n-3].is("with") && selectTokens[n-2].is("no") && selectTokens[n-1].is("data") {
		noData = true
		selectTokens = selectTokens[:n-3]
	} else if n >= 2 && selectTokens[n-2].is("with") && selectTokens[n-1].is("data") {
		selectTokens = selectTokens[:n-2]
	}

	format := strings.ToLower(props["format"])
	if format == formatJSON {
		format = formatJSONFile
	}

	if format != formatParquet && format != formatJSONFile && format != formatTextfile {
		return nil, 0, fmt.Errorf("NOT_SUPPORTED: CTAS format %s", format)
	}

	if len(props["field_delimiter"]) != 1 {
		return nil, 0, fmt.Errorf("field_delimiter must be a single character")
	}

	_, lookupErr := s.catalog.GetTable(ctx, &glueapi.GetTableRequest{DatabaseName: new(db), Name: new(name)})
	if lookupErr == nil {
		return nil, 0, fmt.Errorf("TABLE_ALREADY_EXISTS: %s.%s", db, name)
	}

	if !notFoundErr(lookupErr) {
		return nil, 0, lookupErr
	}

	uri := props["external_location"]
	wc, _ := ctx.Value(writeContextKey{}).(writeContext)
	if uri != "" && wc.enforced {
		return nil, 0, fmt.Errorf("the workgroup enforces a query results location; external_location cannot be set")
	}

	if uri == "" {
		uri = strings.TrimSuffix(wc.output, "/") + "/tables/" + wc.id + "/"
	}

	bucket, prefix, err := location(uri)
	if err != nil {
		return nil, 0, err
	}

	existing, err := s.objects.ListObjectsV2(
		ctx,
		&s3api.ListObjectsV2Request{Bucket: new(bucket), Prefix: new(prefix), MaxKeys: new(int64(1))},
	)
	if err != nil {
		return nil, 0, err
	}

	if len(existing.Contents) > 0 {
		return nil, 0, fmt.Errorf("HIVE_PATH_ALREADY_EXISTS: Target directory for table already exists: %s", uri)
	}

	queryCtx := ctx
	if noData {
		queryCtx = context.WithValue(ctx, schemaOnlyKey{}, true)
	}
	rs, scanned, err := s.query(queryCtx, database, queryTokens(selectTokens), params)
	if err != nil {
		return nil, scanned, err
	}

	if len(partitions) >= len(rs.Columns) {
		return nil, scanned, fmt.Errorf("table must contain at least one non-partition column")
	}

	spec := formats[format]
	sd := &glueapi.StorageDescriptor{
		Location:     new("s3://" + bucket + "/" + prefix),
		InputFormat:  new(spec[0]),
		OutputFormat: new(spec[1]),
		SerdeInfo: &glueapi.SerDeInfo{
			SerializationLibrary: new(spec[2]),
			Parameters:           map[string]string{keyFieldDelim: props["field_delimiter"]},
		},
	}

	table := &glueapi.Table{
		Name:              new(name),
		DatabaseName:      new(db),
		StorageDescriptor: sd,
		TableType:         new("EXTERNAL_TABLE"),
		Parameters: map[string]string{
			"EXTERNAL":          "TRUE",
			"write.compression": strings.ToUpper(props["write_compression"]),
		},
	}

	seenColumns := map[string]bool{}
	for i, c := range rs.Columns {
		if seenColumns[strings.ToLower(c.Name)] {
			return nil, scanned, fmt.Errorf("DUPLICATE_COLUMN_NAME: %s", c.Name)
		}

		seenColumns[strings.ToLower(c.Name)] = true
		hiveType := c.HiveType
		if hiveType == "" {
			hiveType = c.Type
		}
		col := glueapi.Column{Name: new(strings.ToLower(c.Name)), Type: new(hiveType)}
		if i >= len(rs.Columns)-len(partitions) {
			if strings.ToLower(c.Name) != partitions[i-(len(rs.Columns)-len(partitions))] {
				return nil, scanned, fmt.Errorf(
					"HIVE_COLUMN_ORDER_MISMATCH: partition columns must be the last columns in the SELECT",
				)
			}

			table.PartitionKeys = append(table.PartitionKeys, col)
		} else {
			sd.Columns = append(sd.Columns, col)
		}
	}

	if noData {
		rs.Rows = nil
	}

	// Validate the complete encoding before creating any objects or catalog entries.
	if validateErr := validateWrite(table, rs.Rows); validateErr != nil {
		return nil, scanned, validateErr
	}

	if writeErr := s.appendRows(ctx, table, rs.Rows, false); writeErr != nil {
		return nil, scanned, writeErr
	}

	_, err = s.catalog.CreateTable(
		ctx,
		&glueapi.CreateTableRequest{
			DatabaseName: new(db),
			TableInput: &glueapi.TableInput{
				Name:              table.Name,
				StorageDescriptor: sd,
				PartitionKeys:     table.PartitionKeys,
				Parameters:        table.Parameters,
				TableType:         table.TableType,
			},
		},
	)
	if err != nil {
		return nil, scanned, err
	}

	if err := s.registerWrittenPartitions(ctx, table, rs.Rows); err != nil {
		return nil, scanned, err
	}

	return updateResult(len(rs.Rows)), scanned, nil
}

func updateResult(n int) *resultSet {
	return &resultSet{
		Columns:     []resultColumn{{Name: "rows", Type: typeBigint}},
		Rows:        [][]*string{{new(strconv.Itoa(n))}},
		UpdateCount: int64(n),
	}
}

func (s *Service) insertInto(
	ctx context.Context,
	database string,
	tokens []token,
	params []string,
) (*resultSet, int64, error) {
	p := &parser{t: tokens}
	if err := p.expect("insert", "into"); err != nil {
		return nil, 0, err
	}

	t, err := s.tableOf(ctx, p, database)
	if err != nil {
		return nil, 0, err
	}

	if t.StorageDescriptor == nil || len(t.StorageDescriptor.Columns) == 0 {
		return nil, 0, fmt.Errorf("table has no data columns")
	}
	if _, err = formatOf(t); err != nil {
		return nil, 0, err
	}

	data, parts := columnsOf(t)
	all := append(append([]column{}, data...), parts...)
	var names []string
	if p.accept("(") {
		for !p.accept(")") {
			n, nameErr := p.name()
			if nameErr != nil {
				return nil, 0, nameErr
			}

			names = append(names, n)
			if !p.accept(",") {
				if expectErr := p.expect(")"); expectErr != nil {
					return nil, 0, expectErr
				}

				break
			}
		}
	} else {
		for _, c := range all {
			names = append(names, c.name)
		}
	}

	mapping := make([]int, len(names))
	seen := map[string]bool{}
	for i, n := range names {
		mapping[i] = -1
		for j, c := range all {
			if c.name == n {
				mapping[i] = j
				break
			}
		}

		if mapping[i] < 0 || seen[n] {
			return nil, 0, fmt.Errorf("invalid or duplicate INSERT column %s", n)
		}

		seen[n] = true
	}

	if p.done() || (!p.peek().is("select") && !p.peek().is("with") && !p.peek().is("values")) {
		return nil, 0, fmt.Errorf("NOT_SUPPORTED: INSERT requires SELECT or VALUES")
	}

	rs, scanned, err := s.query(ctx, database, queryTokens(p.t[p.i:]), params)
	if err != nil {
		return nil, scanned, err
	}

	if len(rs.Columns) != len(names) {
		return nil, scanned, fmt.Errorf("TYPE_MISMATCH: INSERT column count does not match query")
	}

	rows := make([][]*string, len(rs.Rows))
	for i, r := range rs.Rows {
		rows[i] = make([]*string, len(all))
		for j, v := range r {
			rows[i][mapping[j]] = v
		}
	}

	if err := validateWrite(t, rows); err != nil {
		return nil, scanned, err
	}

	if err := s.appendRows(ctx, t, rows, true); err != nil {
		return nil, scanned, err
	}

	return updateResult(len(rows)), scanned, nil
}

func writeSchema(t *glueapi.Table) []tabular.Column {
	var out []tabular.Column
	for _, c := range t.StorageDescriptor.Columns {
		out = append(out, tabular.Column{Name: common.Value(c.Name), Type: common.Value(c.Type)})
	}

	return out
}

func validateWrite(t *glueapi.Table, rows [][]*string) error {
	f, err := formatOf(t)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if len(partitionGroups(t, rows)) > 100 {
		return fmt.Errorf("HIVE_TOO_MANY_OPEN_PARTITIONS: exceeded limit of 100 open writers for partitions")
	}

	return f.Write(&b, writeSchema(t), rows, t.Parameters["write.compression"])
}

func partitionGroups(t *glueapi.Table, rows [][]*string) map[string][][]*string {
	out := map[string][][]*string{}
	count := len(t.StorageDescriptor.Columns)
	for _, r := range rows {
		values := make([]string, len(t.PartitionKeys))
		for i := range values {
			values[i] = common.Value(r[count+i])
			if r[count+i] == nil {
				values[i] = "__HIVE_DEFAULT_PARTITION__"
			}
		}

		key := partitionPath(t.PartitionKeys, values)
		out[key] = append(out[key], r)
	}

	return out
}

func (s *Service) appendRows(ctx context.Context, t *glueapi.Table, rows [][]*string, register bool) error {
	f, err := formatOf(t)
	if err != nil {
		return err
	}

	bucket, prefix, err := location(common.Value(t.StorageDescriptor.Location))
	if err != nil {
		return err
	}

	for dir, group := range partitionGroups(t, rows) {
		destBucket, destPrefix := bucket, prefix+dir
		if register && len(t.PartitionKeys) > 0 {
			values, _ := pathValues(t.PartitionKeys, "", dir)
			existing, err := s.catalog.GetPartition(
				ctx,
				&glueapi.GetPartitionRequest{DatabaseName: t.DatabaseName, TableName: t.Name, PartitionValues: values},
			)
			if err != nil && !notFoundErr(err) {
				return err
			}

			if err == nil && existing.Partition.StorageDescriptor != nil &&
				existing.Partition.StorageDescriptor.Location != nil {
				destBucket, destPrefix, err = location(*existing.Partition.StorageDescriptor.Location)
				if err != nil {
					return err
				}
			}
		}

		var body bytes.Buffer
		if err := f.Write(&body, writeSchema(t), group, t.Parameters["write.compression"]); err != nil {
			return err
		}

		ext := ".txt"
		switch f.Kind {
		case tabular.Parquet:
			ext = ".parquet"
		case formatJSON:
			ext = ".json"
		}

		compression := t.Parameters["write.compression"]
		if f.Kind != tabular.Parquet && (compression == "" || strings.EqualFold(compression, "GZIP")) {
			ext += ".gz"
		}

		key := destPrefix + common.RandomID(24) + ext
		if _, err := s.objects.PutObject(
			ctx,
			&s3api.PutObjectRequest{Bucket: new(destBucket), Key: new(key), Body: body.Bytes()},
		); err != nil {
			return err
		}
	}

	if register {
		return s.registerWrittenPartitions(ctx, t, rows)
	}

	return nil
}

func (s *Service) registerWrittenPartitions(ctx context.Context, t *glueapi.Table, rows [][]*string) error {
	if len(t.PartitionKeys) == 0 {
		return nil
	}

	for dir := range partitionGroups(t, rows) {
		values, _ := pathValues(t.PartitionKeys, "", dir)
		sd := common.Clone(t.StorageDescriptor)
		sd.Location = new(strings.TrimSuffix(common.Value(sd.Location), "/") + "/" + dir)
		_, err := s.catalog.CreatePartition(
			ctx,
			&glueapi.CreatePartitionRequest{
				DatabaseName:   t.DatabaseName,
				TableName:      t.Name,
				PartitionInput: &glueapi.PartitionInput{Values: values, StorageDescriptor: sd},
			},
		)
		if err != nil && !existsErr(err) {
			return err
		}
	}

	return nil
}
