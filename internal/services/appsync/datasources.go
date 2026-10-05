package appsync

import (
	"context"
	"regexp"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	sourceLambda      = "AWS_LAMBDA"
	sourceDynamoDB    = "AMAZON_DYNAMODB"
	sourceNone        = "NONE"
	sourceHTTP        = "HTTP"
	sourceEventBridge = "AMAZON_EVENTBRIDGE"
)

// resourceName is the pattern of data source, type, and field names.
var resourceName = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*$`)

func checkName(what, name string) error {
	if !resourceName.MatchString(name) {
		return badRequest("%s %q must match [_A-Za-z][_0-9A-Za-z]*.", what, name)
	}

	return nil
}

// checkDataSource validates a data source's type and that it has exactly the
// configuration its type needs.
func checkDataSource(d *api.DataSource) error {
	if err := checkName("Data source name", common.Value(d.Name)); err != nil {
		return err
	}

	configs := map[string]bool{
		sourceLambda: d.LambdaConfig != nil, sourceDynamoDB: d.DynamodbConfig != nil, sourceHTTP: d.HttpConfig != nil,
		sourceEventBridge: d.EventBridgeConfig != nil,
	}
	kind := common.Value(d.Type)
	if _, ok := configs[kind]; !ok && kind != sourceNone {
		return badRequest("TinyStack does not support data sources of type %s.", kind)
	}

	for k, set := range configs {
		if set != (k == kind) {
			return badRequest("A data source of type %s needs exactly the configuration of its type.", kind)
		}
	}

	switch {
	case d.LambdaConfig != nil && common.Value(d.LambdaConfig.LambdaFunctionArn) == "":
		return badRequest("lambdaConfig.lambdaFunctionArn is required.")
	case d.DynamodbConfig != nil && (common.Value(d.DynamodbConfig.TableName) == "" ||
		common.Value(d.DynamodbConfig.AwsRegion) == ""):
		return badRequest("dynamodbConfig needs a tableName and an awsRegion.")
	case d.HttpConfig != nil && common.Value(d.HttpConfig.Endpoint) == "":
		return badRequest("httpConfig.endpoint is required.")
	case d.EventBridgeConfig != nil && common.Value(d.EventBridgeConfig.EventBusArn) == "":
		return badRequest("eventBridgeConfig.eventBusArn is required.")
	}

	return nil
}

func loadDataSource(tx storage.ReadTx, scope request.Scope, apiID, name *string) (*api.DataSource, error) {
	d, ok, err := storage.Read[api.DataSource](tx,
		owned(scope, kindDataSource, common.Value(apiID), common.Value(name)))
	if err != nil || !ok {
		return nil, common.ErrorsOr(err, notFound("Data source %s not found.", common.Value(name)))
	}

	return d, nil
}

// putDataSource validates d and stores it under the API apiID names, which
// must have no data source with its name unless replace is set.
func (s *Service) putDataSource(ctx context.Context, apiID *string, d *api.DataSource, replace bool) error {
	if err := checkDataSource(d); err != nil {
		return err
	}

	return s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadAPI(tx, scope, apiID); err != nil {
			return err
		}

		_, err := loadDataSource(tx, scope, apiID, d.Name)
		switch {
		case replace && err != nil:
			return err
		case !replace && err == nil:
			return badRequest("Data source with name %s already exists.", *d.Name)
		}

		d.DataSourceArn = new(apiARN(scope, *apiID) + "/datasources/" + *d.Name)
		return storage.Write(tx, owned(scope, kindDataSource, *apiID, *d.Name), d)
	})
}

func (s *Service) CreateDataSource(
	ctx context.Context,
	in *api.CreateDataSourceRequest,
) (*api.CreateDataSourceResponse, error) {
	d := &api.DataSource{
		Name: in.Name, Description: in.Description, Type: in.Type, ServiceRoleArn: in.ServiceRoleArn,
		DynamodbConfig: in.DynamodbConfig, LambdaConfig: in.LambdaConfig, HttpConfig: in.HttpConfig,
		EventBridgeConfig: in.EventBridgeConfig,
	}
	if err := s.putDataSource(ctx, in.ApiId, d, false); err != nil {
		return nil, err
	}

	return &api.CreateDataSourceResponse{DataSource: d}, nil
}

func (s *Service) UpdateDataSource(
	ctx context.Context,
	in *api.UpdateDataSourceRequest,
) (*api.UpdateDataSourceResponse, error) {
	d := &api.DataSource{
		Name: in.Name, Description: in.Description, Type: in.Type, ServiceRoleArn: in.ServiceRoleArn,
		DynamodbConfig: in.DynamodbConfig, LambdaConfig: in.LambdaConfig, HttpConfig: in.HttpConfig,
		EventBridgeConfig: in.EventBridgeConfig,
	}
	if err := s.putDataSource(ctx, in.ApiId, d, true); err != nil {
		return nil, err
	}

	return &api.UpdateDataSourceResponse{DataSource: d}, nil
}

func (s *Service) GetDataSource(ctx context.Context, in *api.GetDataSourceRequest) (*api.GetDataSourceResponse, error) {
	out := &api.GetDataSourceResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		var err error
		out.DataSource, err = loadDataSource(tx, scope, in.ApiId, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteDataSource(
	ctx context.Context,
	in *api.DeleteDataSourceRequest,
) (*api.DeleteDataSourceResponse, error) {
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		if _, err := loadDataSource(tx, scope, in.ApiId, in.Name); err != nil {
			return err
		}

		tx.Delete(owned(scope, kindDataSource, *in.ApiId, *in.Name))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteDataSourceResponse{}, nil
}

func (s *Service) ListDataSources(
	ctx context.Context,
	in *api.ListDataSourcesRequest,
) (*api.ListDataSourcesResponse, error) {
	out := &api.ListDataSourcesResponse{}
	err := s.view(ctx, func(tx storage.ReadTx, scope request.Scope) error {
		if _, err := loadAPI(tx, scope, in.ApiId); err != nil {
			return err
		}

		all, err := records[api.DataSource](tx, owned(scope, kindDataSource, *in.ApiId, ""))
		if err != nil {
			return err
		}

		out.DataSources, out.NextToken, err = page(all, func(d api.DataSource) string { return *d.Name },
			in.NextToken, in.MaxResults)
		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
