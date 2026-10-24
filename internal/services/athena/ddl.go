package athena

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/common"
)

// The statement types Athena reports.
const (
	statementDML     = "DML"
	statementDDL     = "DDL"
	statementUtility = "UTILITY"
)

// parser reads a statement's tokens, without whitespace.
type parser struct {
	t []token
	i int
}

func (p *parser) peek() token {
	if p.i < len(p.t) {
		return p.t[p.i]
	}

	return token{}
}

// accept consumes words, in order, if the statement continues with them.
func (p *parser) accept(words ...string) bool {
	for k, w := range words {
		if p.i+k >= len(p.t) || !p.t[p.i+k].is(w) && p.t[p.i+k].text != w {
			return false
		}
	}

	p.i += len(words)
	return true
}

func (p *parser) expect(words ...string) error {
	if !p.accept(words...) {
		return fmt.Errorf("line 1: expected %s near %q", strings.Join(words, " "), p.peek().text)
	}

	return nil
}

func (p *parser) done() bool { return p.i >= len(p.t) }

// name reads an identifier.
func (p *parser) name() (string, error) {
	n, ok := p.peek().identifier()
	if !ok {
		return "", fmt.Errorf("line 1: expected a name near %q", p.peek().text)
	}

	p.i++
	return n, nil
}

// qualified reads [database.]table, in database when it names none.
func (p *parser) qualified(database string) (db, table string, err error) {
	first, err := p.name()
	if err != nil {
		return "", "", err
	}

	if !p.accept(".") {
		return database, first, nil
	}

	table, err = p.name()
	return first, table, err
}

// str reads a string literal, with Hive's escapes such as \t and \001.
func (p *parser) str() (string, error) {
	t := p.peek()
	if t.kind != tokenString {
		return "", fmt.Errorf("line 1: expected a string near %q", t.text)
	}

	p.i++
	return unescape(t.value), nil
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}

		i++
		switch c := s[i]; {
		case c == 't':
			b.WriteByte('\t')
		case c == 'n':
			b.WriteByte('\n')
		case c == 'r':
			b.WriteByte('\r')
		case c >= '0' && c <= '7' && i+2 < len(s):
			if n, err := strconv.ParseUint(s[i:i+3], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}

			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}

	return b.String()
}

// properties reads ('key' = 'value', ...).
func (p *parser) properties() (map[string]string, error) {
	out := map[string]string{}
	if err := p.expect("("); err != nil {
		return nil, err
	}

	for !p.accept(")") {
		k, err := p.str()
		if err != nil {
			return nil, err
		}

		if expectErr := p.expect("="); expectErr != nil {
			return nil, expectErr
		}

		if out[k], err = p.str(); err != nil {
			return nil, err
		}

		p.accept(",")
	}

	return out, nil
}

// columns reads (name type [COMMENT '...'], ...), where a type such as
// array<struct<a:int>> or decimal(10,2) spans several tokens.
func (p *parser) columns() ([]glueapi.Column, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}

	var out []glueapi.Column
	for !p.accept(")") {
		n, err := p.name()
		if err != nil {
			return nil, err
		}

		var kind strings.Builder
		for depth := 0; !p.done(); p.i++ {
			t := p.peek()
			if depth == 0 && (t.text == "," || t.text == ")" || t.is("comment")) {
				break
			}

			switch t.text {
			case "<", "(":
				depth++
			case ">", ")":
				depth--
			}

			kind.WriteString(strings.ToLower(t.text))
		}

		c := glueapi.Column{Name: new(n), Type: new(kind.String())}
		if p.accept("comment") {
			comment, commentErr := p.str()
			if commentErr != nil {
				return nil, commentErr
			}

			c.Comment = new(comment)
		}

		if kind.Len() == 0 {
			return nil, fmt.Errorf("line 1: the column %s has no type", n)
		}

		out = append(out, c)
		p.accept(",")
	}

	return out, nil
}

// like compiles a Hive name pattern: * matches anything and | separates
// alternatives.
func like(pattern string) *regexp.Regexp {
	alternatives := strings.Split(pattern, "|")
	for i, a := range alternatives {
		alternatives[i] = strings.ReplaceAll(regexp.QuoteMeta(strings.TrimSpace(a)), `\*`, ".*")
	}

	return regexp.MustCompile("(?i)^(?:" + strings.Join(alternatives, "|") + ")$")
}

func notFoundErr(err error) bool {
	ae, ok := errors.AsType[*awserror.Error](err)
	return ok && ae.Code == "EntityNotFoundException"
}

func existsErr(err error) bool {
	ae, ok := errors.AsType[*awserror.Error](err)
	return ok && ae.Code == "AlreadyExistsException"
}

// lines is a DDL statement's result: one varchar column.
func lines(name string, values ...string) *resultSet {
	rs := &resultSet{Columns: []resultColumn{{Name: name, Type: typeVarchar}}}
	for _, v := range values {
		rs.Rows = append(rs.Rows, []*string{new(v)})
	}

	return rs
}

// ddl runs a catalog statement in database, returning its result and its
// statement type.
func (s *Service) ddl(ctx context.Context, database string, tokens []token) (*resultSet, string, error) {
	p := &parser{t: solid(tokens)}
	switch {
	case p.accept("create", "database"), p.accept("create", "schema"):
		return lines("result"), statementDDL, s.createDatabase(ctx, p)
	case p.accept("drop", "database"), p.accept("drop", "schema"):
		return lines("result"), statementDDL, s.dropDatabase(ctx, p)
	case p.accept("create", "external", "table"), p.accept("create", "table"):
		return lines("result"), statementDDL, s.createTable(ctx, p, database)
	case p.accept("drop", "table"):
		ifExists := p.accept("if", "exists")
		db, table, err := p.qualified(database)
		if err == nil {
			_, err = s.catalog.DeleteTable(ctx, &glueapi.DeleteTableRequest{DatabaseName: &db, Name: &table})
		}

		if notFoundErr(err) && ifExists {
			err = nil
		}

		return lines("result"), statementDDL, err
	case p.accept("show", "databases"), p.accept("show", "schemas"):
		rs, err := s.showDatabases(ctx, p)
		return rs, statementUtility, err
	case p.accept("show", "tables"):
		rs, err := s.showTables(ctx, p, database)
		return rs, statementUtility, err
	case p.accept("show", "columns", "in"), p.accept("show", "columns", "from"), p.accept("describe"),
		p.accept("desc"):
		describe := !p.t[0].is("show")
		p.accept("formatted")
		p.accept("extended")
		t, err := s.tableOf(ctx, p, database)
		if err != nil {
			return nil, statementUtility, err
		}

		var out []string
		all := append(append([]glueapi.Column{}, common.OrElse(t.StorageDescriptor,
			&glueapi.StorageDescriptor{}).Columns...), t.PartitionKeys...)
		for _, c := range all {
			line := common.Value(c.Name)
			if describe {
				line += "\t" + common.Value(c.Type) + "\t" + common.Value(c.Comment)
			}

			out = append(out, line)
		}

		return lines(map[bool]string{true: "col_name", false: "field"}[describe], out...), statementUtility, nil
	case p.accept("show", "partitions"):
		rs, err := s.showPartitions(ctx, p, database)
		return rs, statementUtility, err
	case p.accept("msck", "repair", "table"):
		rs, err := s.repair(ctx, p, database)
		return rs, statementDDL, err
	case p.accept("alter", "table"):
		return lines("result"), statementDDL, s.alterPartitions(ctx, p, database)
	}

	return nil, statementDDL, fmt.Errorf("NOT_SUPPORTED: TinyStack does not run this statement: %s",
		strings.TrimSpace(firstWords(tokens, 3)))
}

func firstWords(tokens []token, n int) string {
	var words []string
	for _, t := range solid(tokens) {
		if len(words) == n {
			break
		}

		words = append(words, t.text)
	}

	return strings.Join(words, " ")
}

func (s *Service) createDatabase(ctx context.Context, p *parser) error {
	ifNotExists := p.accept("if", "not", "exists")
	n, err := p.name()
	if err != nil {
		return err
	}

	in := &glueapi.DatabaseInput{Name: new(n)}
	for !p.done() {
		switch {
		case p.accept("comment"):
			c, strErr := p.str()
			in.Description, err = new(c), strErr
		case p.accept("location"):
			l, strErr := p.str()
			in.LocationUri, err = new(l), strErr
		case p.accept("with", "dbproperties"):
			in.Parameters, err = p.properties()
		default:
			return fmt.Errorf("line 1: unexpected %q", p.peek().text)
		}

		if err != nil {
			return err
		}
	}

	_, err = s.catalog.CreateDatabase(ctx, &glueapi.CreateDatabaseRequest{DatabaseInput: in})
	if existsErr(err) && ifNotExists {
		return nil
	}

	return err
}

func (s *Service) dropDatabase(ctx context.Context, p *parser) error {
	ifExists := p.accept("if", "exists")
	n, err := p.name()
	if err != nil {
		return err
	}

	cascade := p.accept("cascade")
	p.accept("restrict")
	tables, err := s.catalog.GetTables(ctx, &glueapi.GetTablesRequest{DatabaseName: &n})
	switch {
	case notFoundErr(err) && ifExists:
		return nil
	case err != nil:
		return err
	case len(tables.TableList) > 0 && !cascade:
		return fmt.Errorf("InvalidOperationException: Database %s is not empty. One or more tables exist", n)
	}

	_, err = s.catalog.DeleteDatabase(ctx, &glueapi.DeleteDatabaseRequest{Name: &n})
	return err
}

// formats are the input and output formats STORED AS names.
var formats = map[string][3]string{
	formatTextfile: {
		"org.apache.hadoop.mapred.TextInputFormat", "org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat",
		"org.apache.hadoop.hive.serde2.lazy.LazySimpleSerDe",
	},
	formatParquet: {
		"org.apache.hadoop.hive.ql.io.parquet.MapredParquetInputFormat",
		"org.apache.hadoop.hive.ql.io.parquet.MapredParquetOutputFormat",
		"org.apache.hadoop.hive.ql.io.parquet.serde.ParquetHiveSerDe",
	},
	"orc": {
		"org.apache.hadoop.hive.ql.io.orc.OrcInputFormat", "org.apache.hadoop.hive.ql.io.orc.OrcOutputFormat",
		"org.apache.hadoop.hive.ql.io.orc.OrcSerde",
	},
	formatJSONFile: {
		"org.apache.hadoop.mapred.TextInputFormat", "org.apache.hadoop.hive.ql.io.HiveIgnoreKeyTextOutputFormat",
		"org.openx.data.jsonserde.JsonSerDe",
	},
}

// createTable runs CREATE [EXTERNAL] TABLE with Hive's clauses: columns,
// COMMENT, PARTITIONED BY, ROW FORMAT DELIMITED or SERDE, STORED AS,
// LOCATION, and TBLPROPERTIES.
func (s *Service) createTable(ctx context.Context, p *parser, database string) error {
	ifNotExists := p.accept("if", "not", "exists")
	db, n, err := p.qualified(database)
	if err != nil {
		return err
	}

	if p.peek().is("as") || p.peek().is("with") {
		return errors.New("NOT_SUPPORTED: TinyStack does not run CREATE TABLE AS SELECT")
	}

	sd := &glueapi.StorageDescriptor{
		InputFormat: new(formats[formatTextfile][0]), OutputFormat: new(formats[formatTextfile][1]),
		SerdeInfo: &glueapi.SerDeInfo{
			SerializationLibrary: new(formats[formatTextfile][2]), Parameters: map[string]string{},
		},
	}
	in := &glueapi.TableInput{
		Name: new(n), TableType: new("EXTERNAL_TABLE"), StorageDescriptor: sd,
		Parameters: map[string]string{"EXTERNAL": "TRUE"},
	}
	if sd.Columns, err = p.columns(); err != nil {
		return err
	}

	for !p.done() {
		switch {
		case p.accept("comment"):
			c, strErr := p.str()
			in.Description, err = new(c), strErr
		case p.accept("partitioned", "by"):
			in.PartitionKeys, err = p.columns()
		case p.accept("row", "format", "delimited"):
			err = p.delimited(sd.SerdeInfo.Parameters)
		case p.accept("row", "format", "serde"):
			lib, strErr := p.str()
			sd.SerdeInfo.SerializationLibrary, err = new(lib), strErr
			if err == nil && p.accept("with", "serdeproperties") {
				var props map[string]string
				if props, err = p.properties(); err == nil {
					sd.SerdeInfo.Parameters = props
				}
			}
		case p.accept("stored", "as", "inputformat"):
			var input, output string
			if input, err = p.str(); err == nil {
				if err = p.expect("outputformat"); err == nil {
					output, err = p.str()
				}
			}

			sd.InputFormat, sd.OutputFormat = new(input), new(output)
		case p.accept("stored", "as"):
			var kind string
			if kind, err = p.name(); err == nil {
				f, known := formats[kind]
				if !known {
					return fmt.Errorf("NOT_SUPPORTED: TinyStack does not know STORED AS %s", kind)
				}

				sd.InputFormat, sd.OutputFormat = new(f[0]), new(f[1])
				if kind != formatTextfile {
					sd.SerdeInfo.SerializationLibrary = new(f[2])
				}
			}
		case p.accept("location"):
			l, strErr := p.str()
			sd.Location, err = new(l), strErr
		case p.accept("tblproperties"):
			var props map[string]string
			if props, err = p.properties(); err == nil {
				maps.Copy(in.Parameters, props)
			}
		default:
			return fmt.Errorf("line 1: unexpected %q", p.peek().text)
		}

		if err != nil {
			return err
		}
	}

	if strings.EqualFold(in.Parameters["table_type"], "iceberg") {
		return errors.New("NOT_SUPPORTED: TinyStack does not create Iceberg tables")
	}

	if sd.Location == nil {
		return errors.New("line 1: an external table needs a LOCATION")
	}

	_, err = s.catalog.CreateTable(ctx, &glueapi.CreateTableRequest{DatabaseName: &db, TableInput: in})
	if existsErr(err) && ifNotExists {
		return nil
	}

	return err
}

// delimited reads ROW FORMAT DELIMITED's clauses into SerDe parameters.
func (p *parser) delimited(params map[string]string) error {
	for {
		var key string
		switch {
		case p.accept("fields", "terminated", "by"):
			key = keyFieldDelim
		case p.accept("escaped", "by"):
			key = "escape.delim"
		case p.accept("collection", "items", "terminated", "by"):
			key = "collection.delim"
		case p.accept("map", "keys", "terminated", "by"):
			key = "mapkey.delim"
		case p.accept("lines", "terminated", "by"):
			key = "line.delim"
		case p.accept("null", "defined", "as"):
			key = "serialization.null.format"
		default:
			return nil
		}

		v, err := p.str()
		if err != nil {
			return err
		}

		params[key] = v
		if key == keyFieldDelim {
			params["serialization.format"] = v
		}
	}
}

func (s *Service) showDatabases(ctx context.Context, p *parser) (*resultSet, error) {
	var pattern *regexp.Regexp
	if p.accept("like") || p.peek().kind == tokenString {
		v, err := p.str()
		if err != nil {
			return nil, err
		}

		pattern = like(v)
	}

	all, err := s.catalog.GetDatabases(ctx, &glueapi.GetDatabasesRequest{})
	if err != nil {
		return nil, err
	}

	var names []string
	for _, d := range all.DatabaseList {
		if pattern == nil || pattern.MatchString(*d.Name) {
			names = append(names, *d.Name)
		}
	}

	return lines("database_name", names...), nil
}

func (s *Service) showTables(ctx context.Context, p *parser, database string) (*resultSet, error) {
	if p.accept("in") || p.accept("from") {
		db, err := p.name()
		if err != nil {
			return nil, err
		}

		database = db
	}

	var pattern *regexp.Regexp
	if p.accept("like") || p.peek().kind == tokenString {
		v, err := p.str()
		if err != nil {
			return nil, err
		}

		pattern = like(v)
	}

	all, err := s.catalog.GetTables(ctx, &glueapi.GetTablesRequest{DatabaseName: &database})
	if err != nil {
		return nil, err
	}

	var names []string
	for i := range all.TableList {
		if n := all.TableList[i].Name; pattern == nil || pattern.MatchString(*n) {
			names = append(names, *n)
		}
	}

	return lines("tab_name", names...), nil
}

func (s *Service) tableOf(ctx context.Context, p *parser, database string) (*glueapi.Table, error) {
	db, n, err := p.qualified(database)
	if err != nil {
		return nil, err
	}

	t, err := s.catalog.GetTable(ctx, &glueapi.GetTableRequest{DatabaseName: &db, Name: &n})
	if err != nil {
		return nil, err
	}

	return t.Table, nil
}

func (s *Service) showPartitions(ctx context.Context, p *parser, database string) (*resultSet, error) {
	t, err := s.tableOf(ctx, p, database)
	if err != nil {
		return nil, err
	}

	all, err := s.catalog.GetPartitions(ctx, &glueapi.GetPartitionsRequest{
		DatabaseName: t.DatabaseName, TableName: t.Name,
	})
	if err != nil {
		return nil, err
	}

	var out []string
	for _, part := range all.Partitions {
		out = append(out, strings.TrimSuffix(partitionPath(t.PartitionKeys, part.Values), "/"))
	}

	return lines("partition", out...), nil
}

// repair registers the partitions whose key=value directories a table's
// location holds, as MSCK REPAIR TABLE does.
func (s *Service) repair(ctx context.Context, p *parser, database string) (*resultSet, error) {
	t, err := s.tableOf(ctx, p, database)
	if err != nil {
		return nil, err
	}

	if len(t.PartitionKeys) == 0 {
		return lines("result"), nil
	}

	bucket, prefix, err := location(common.Value(common.OrElse(t.StorageDescriptor,
		&glueapi.StorageDescriptor{}).Location))
	if err != nil {
		return nil, err
	}

	objects, err := s.objectsOf(ctx, t, source{bucket: bucket, prefix: prefix})
	if err != nil {
		return nil, err
	}

	var added []string
	for _, o := range objects {
		dir := partitionPath(t.PartitionKeys, o.values)
		if slices.Contains(added, dir) {
			continue
		}

		partition := *common.Clone(t.StorageDescriptor)
		partition.Location = new("s3://" + bucket + "/" + prefix + dir)
		_, createErr := s.catalog.CreatePartition(ctx, &glueapi.CreatePartitionRequest{
			DatabaseName: t.DatabaseName, TableName: t.Name,
			PartitionInput: &glueapi.PartitionInput{Values: o.values, StorageDescriptor: &partition},
		})
		switch {
		case existsErr(createErr):
		case createErr != nil:
			return nil, createErr
		default:
			added = append(added, dir)
		}
	}

	out := make([]string, 0, len(added))
	for _, dir := range added {
		out = append(out, "Repair: Added partition to metastore "+*t.Name+":"+strings.TrimSuffix(dir, "/"))
	}

	return lines("result", out...), nil
}

// partitionSpec reads PARTITION (k = 'v', ...) as values in the order of a
// table's partition keys.
func (p *parser) partitionSpec(t *glueapi.Table) ([]string, error) {
	if err := p.expect("partition", "("); err != nil {
		return nil, err
	}

	given := map[string]string{}
	for !p.accept(")") {
		k, err := p.name()
		if err != nil {
			return nil, err
		}

		if expectErr := p.expect("="); expectErr != nil {
			return nil, expectErr
		}

		v := p.peek()
		p.i++
		given[k] = unescape(v.value)
		if v.kind == tokenNumber {
			given[k] = v.text
		}

		p.accept(",")
	}

	values := make([]string, len(t.PartitionKeys))
	for i, k := range t.PartitionKeys {
		v, ok := given[strings.ToLower(common.Value(k.Name))]
		if !ok || len(given) != len(t.PartitionKeys) {
			return nil, fmt.Errorf("line 1: the partition must give each partition key a value")
		}

		values[i] = v
	}

	return values, nil
}

// alterPartitions runs ALTER TABLE ... ADD [IF NOT EXISTS] PARTITION (...)
// [LOCATION '...'] and DROP [IF EXISTS] PARTITION (...), which may repeat.
func (s *Service) alterPartitions(ctx context.Context, p *parser, database string) error {
	t, err := s.tableOf(ctx, p, database)
	if err != nil {
		return err
	}

	switch {
	case p.accept("add"):
		ifNotExists := p.accept("if", "not", "exists")
		for !p.done() {
			values, specErr := p.partitionSpec(t)
			if specErr != nil {
				return specErr
			}

			partition := *common.Clone(t.StorageDescriptor)
			partition.Location = new(strings.TrimSuffix(common.Value(partition.Location), "/") + "/" +
				partitionPath(t.PartitionKeys, values))
			if p.accept("location") {
				l, strErr := p.str()
				if strErr != nil {
					return strErr
				}

				partition.Location = new(l)
			}

			_, err = s.catalog.CreatePartition(ctx, &glueapi.CreatePartitionRequest{
				DatabaseName: t.DatabaseName, TableName: t.Name,
				PartitionInput: &glueapi.PartitionInput{Values: values, StorageDescriptor: &partition},
			})
			if err != nil && (!existsErr(err) || !ifNotExists) {
				return err
			}
		}

		return nil
	case p.accept("drop"):
		ifExists := p.accept("if", "exists")
		for !p.done() {
			values, specErr := p.partitionSpec(t)
			if specErr != nil {
				return specErr
			}

			_, err = s.catalog.DeletePartition(ctx, &glueapi.DeletePartitionRequest{
				DatabaseName: t.DatabaseName, TableName: t.Name, PartitionValues: values,
			})
			if err != nil && (!notFoundErr(err) || !ifExists) {
				return err
			}

			p.accept(",")
		}

		return nil
	}

	return errors.New("NOT_SUPPORTED: TinyStack runs ALTER TABLE ADD PARTITION and DROP PARTITION only")
}
