package athena

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	"github.com/amjadjibon/tinystack/internal/common"
)

// checkCatalog checks that a request names AwsDataCatalog, the Glue Data
// Catalog of the caller's account and the only catalog TinyStack has.
func checkCatalog(name *string) error {
	if !strings.EqualFold(common.Value(name), catalogName) {
		return invalid("TinyStack has only the %s data catalog, not %s.", catalogName, common.Value(name))
	}

	return nil
}

// metadata turns Glue's errors into Athena's.
func metadata(err error) error {
	if ae, ok := errors.AsType[*awserror.Error](err); ok && ae.Code == "EntityNotFoundException" {
		return awserror.New("MetadataException", ae.Message)
	}

	return err
}

func (s *Service) GetDataCatalog(ctx context.Context, in *api.GetDataCatalogInput) (*api.GetDataCatalogOutput, error) {
	if err := checkCatalog(in.Name); err != nil {
		return nil, err
	}

	return &api.GetDataCatalogOutput{DataCatalog: &api.DataCatalog{
		Name: new(catalogName), Type: new("GLUE"), Description: new("The AWS Glue Data Catalog of the account."),
		Parameters: map[string]string{"catalog-id": accountOf(ctx)},
	}}, nil
}

func (s *Service) ListDataCatalogs(
	_ context.Context,
	_ *api.ListDataCatalogsInput,
) (*api.ListDataCatalogsOutput, error) {
	return &api.ListDataCatalogsOutput{DataCatalogsSummary: []api.DataCatalogSummary{
		{CatalogName: new(catalogName), Type: new("GLUE")},
	}}, nil
}

func (s *Service) ListDatabases(ctx context.Context, in *api.ListDatabasesInput) (*api.ListDatabasesOutput, error) {
	if err := checkCatalog(in.CatalogName); err != nil {
		return nil, err
	}

	all, err := s.catalog.GetDatabases(ctx, &glueapi.GetDatabasesRequest{})
	if err != nil {
		return nil, metadata(err)
	}

	selected, next, err := page(all.DatabaseList, func(d glueapi.Database) string { return *d.Name }, in.NextToken,
		in.MaxResults, 50, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListDatabasesOutput{DatabaseList: []api.Database{}, NextToken: next}
	for _, d := range selected {
		out.DatabaseList = append(out.DatabaseList, api.Database{
			Name: d.Name, Description: d.Description, Parameters: d.Parameters,
		})
	}

	return out, nil
}

func (s *Service) GetDatabase(ctx context.Context, in *api.GetDatabaseInput) (*api.GetDatabaseOutput, error) {
	if err := checkCatalog(in.CatalogName); err != nil {
		return nil, err
	}

	d, err := s.catalog.GetDatabase(ctx, &glueapi.GetDatabaseRequest{Name: in.DatabaseName})
	if err != nil {
		return nil, metadata(err)
	}

	return &api.GetDatabaseOutput{Database: &api.Database{
		Name: d.Database.Name, Description: d.Database.Description, Parameters: d.Database.Parameters,
	}}, nil
}

// tableMetadata describes a Glue table as Athena does, with its storage in
// its parameters.
func tableMetadata(t *glueapi.Table) api.TableMetadata {
	columns := func(list []glueapi.Column) []api.Column {
		out := []api.Column{}
		for _, c := range list {
			out = append(out, api.Column{Name: c.Name, Type: c.Type, Comment: c.Comment})
		}

		return out
	}
	parameters := maps.Clone(t.Parameters)
	if parameters == nil {
		parameters = map[string]string{}
	}

	sd := common.OrElse(t.StorageDescriptor, &glueapi.StorageDescriptor{})
	for k, v := range map[string]*string{
		"location": sd.Location, "inputformat": sd.InputFormat, "outputformat": sd.OutputFormat,
		"serde.serialization.lib": common.OrElse(sd.SerdeInfo, &glueapi.SerDeInfo{}).SerializationLibrary,
	} {
		if v != nil {
			parameters[k] = *v
		}
	}

	for k, v := range common.OrElse(sd.SerdeInfo, &glueapi.SerDeInfo{}).Parameters {
		parameters["serde.param."+k] = v
	}

	return api.TableMetadata{
		Name: t.Name, CreateTime: t.CreateTime, LastAccessTime: t.LastAccessTime, TableType: t.TableType,
		Columns: columns(sd.Columns), PartitionKeys: columns(t.PartitionKeys), Parameters: parameters,
	}
}

func (s *Service) ListTableMetadata(
	ctx context.Context,
	in *api.ListTableMetadataInput,
) (*api.ListTableMetadataOutput, error) {
	if err := checkCatalog(in.CatalogName); err != nil {
		return nil, err
	}

	var pattern *regexp.Regexp
	if in.Expression != nil {
		var err error
		if pattern, err = regexp.Compile("^(?:" + *in.Expression + ")$"); err != nil {
			return nil, invalid("Invalid Expression %s.", *in.Expression)
		}
	}

	all, err := s.catalog.GetTables(ctx, &glueapi.GetTablesRequest{DatabaseName: in.DatabaseName})
	if err != nil {
		return nil, metadata(err)
	}

	var tables []glueapi.Table
	for i := range all.TableList {
		if t := all.TableList[i]; pattern == nil || pattern.MatchString(*t.Name) {
			tables = append(tables, t)
		}
	}

	selected, next, err := page(tables, func(t glueapi.Table) string { return *t.Name }, in.NextToken,
		in.MaxResults, 50, 50)
	if err != nil {
		return nil, err
	}

	out := &api.ListTableMetadataOutput{TableMetadataList: []api.TableMetadata{}, NextToken: next}
	for i := range selected {
		out.TableMetadataList = append(out.TableMetadataList, tableMetadata(&selected[i]))
	}

	return out, nil
}

func (s *Service) GetTableMetadata(
	ctx context.Context,
	in *api.GetTableMetadataInput,
) (*api.GetTableMetadataOutput, error) {
	if err := checkCatalog(in.CatalogName); err != nil {
		return nil, err
	}

	t, err := s.catalog.GetTable(ctx, &glueapi.GetTableRequest{DatabaseName: in.DatabaseName, Name: in.TableName})
	if err != nil {
		return nil, metadata(err)
	}

	m := tableMetadata(t.Table)
	return &api.GetTableMetadataOutput{TableMetadata: &m}, nil
}
