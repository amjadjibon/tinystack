package databasemigrationservice

import (
	"context"
	"slices"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var engines = []string{
	"mysql", "postgres", "aurora", "aurora-postgresql", "mariadb", "oracle", "sqlserver",
	"sybase", "db2", "db2-zos", "mongodb", "docdb", "s3", "dynamodb", "kinesis", "kafka", "elasticsearch",
	"redshift", "neptune", "redis", "gcp-mysql", "timestream",
}

func checkEndpoint(e *api.Endpoint) error {
	redactEndpoint(e)

	if !slices.Contains(engines, common.Value(e.EngineName)) {
		return invalid("Unsupported endpoint engine.")
	}

	if !slices.Contains([]string{"source", "target"}, common.Value(e.EndpointType)) {
		return invalid("EndpointType must be source or target.")
	}

	if e.Port != nil && (*e.Port < 1 || *e.Port > 65535) {
		return invalid("Port must be between 1 and 65535.")
	}

	if !slices.Contains([]string{"none", "require", "verify-ca", "verify-full"}, common.Value(e.SslMode)) {
		return invalid("Invalid SslMode.")
	}

	return nil
}

func (s *Service) CreateEndpoint(ctx context.Context, in *api.CreateEndpointMessage,
) (*api.CreateEndpointResponse, error) {
	r, err := s.create(ctx, kindEndpoint, common.Value(in.EndpointIdentifier), in.Tags,
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			e := &api.Endpoint{EndpointArn: new(r.ARN), Status: new("active")}
			e.EndpointIdentifier = in.EndpointIdentifier
			e.EndpointType = in.EndpointType
			e.EngineName = in.EngineName
			e.Username = in.Username
			e.ServerName = in.ServerName
			e.Port = in.Port
			e.DatabaseName = in.DatabaseName
			e.ExtraConnectionAttributes = in.ExtraConnectionAttributes
			e.KmsKeyId = in.KmsKeyId
			e.SslMode = in.SslMode
			e.ServiceAccessRoleArn = in.ServiceAccessRoleArn
			e.ExternalTableDefinition = in.ExternalTableDefinition
			e.DynamoDbSettings = in.DynamoDbSettings
			e.S3Settings = in.S3Settings
			e.MongoDbSettings = in.MongoDbSettings
			e.KinesisSettings = in.KinesisSettings
			e.KafkaSettings = in.KafkaSettings
			e.ElasticsearchSettings = in.ElasticsearchSettings
			e.NeptuneSettings = in.NeptuneSettings
			e.RedshiftSettings = in.RedshiftSettings
			e.PostgreSQLSettings = in.PostgreSQLSettings
			e.MySQLSettings = in.MySQLSettings
			e.OracleSettings = in.OracleSettings
			e.SybaseSettings = in.SybaseSettings
			e.MicrosoftSQLServerSettings = in.MicrosoftSQLServerSettings
			e.IBMDb2Settings = in.IBMDb2Settings
			e.DocDbSettings = in.DocDbSettings
			e.RedisSettings = in.RedisSettings
			e.GcpMySQLSettings = in.GcpMySQLSettings
			e.TimestreamSettings = in.TimestreamSettings
			e.SslMode = common.OrElse(e.SslMode, new("none"))
			if err := checkEndpoint(e); err != nil {
				return err
			}

			r.Endpoint = e
			return nil
		})
	if err != nil {
		return nil, err
	}

	return &api.CreateEndpointResponse{Endpoint: r.Endpoint}, nil
}

func (s *Service) ModifyEndpoint(ctx context.Context, in *api.ModifyEndpointMessage,
) (*api.ModifyEndpointResponse, error) {
	r, err := s.change(ctx, kindEndpoint, common.Value(in.EndpointArn),
		func(_ storage.WriteTx, _ request.Scope, r *record) error {
			e := r.Endpoint
			e.EndpointIdentifier = common.OrElse(in.EndpointIdentifier, e.EndpointIdentifier)
			e.EndpointType = common.OrElse(in.EndpointType, e.EndpointType)
			e.EngineName = common.OrElse(in.EngineName, e.EngineName)
			e.Username = common.OrElse(in.Username, e.Username)
			e.ServerName = common.OrElse(in.ServerName, e.ServerName)
			e.Port = common.OrElse(in.Port, e.Port)
			e.DatabaseName = common.OrElse(in.DatabaseName, e.DatabaseName)
			e.ExtraConnectionAttributes = common.OrElse(in.ExtraConnectionAttributes, e.ExtraConnectionAttributes)
			e.SslMode = common.OrElse(in.SslMode, e.SslMode)
			e.ServiceAccessRoleArn = common.OrElse(in.ServiceAccessRoleArn, e.ServiceAccessRoleArn)
			e.ExternalTableDefinition = common.OrElse(in.ExternalTableDefinition, e.ExternalTableDefinition)
			e.DynamoDbSettings = common.OrElse(in.DynamoDbSettings, e.DynamoDbSettings)
			e.S3Settings = common.OrElse(in.S3Settings, e.S3Settings)
			e.MongoDbSettings = common.OrElse(in.MongoDbSettings, e.MongoDbSettings)
			e.KinesisSettings = common.OrElse(in.KinesisSettings, e.KinesisSettings)
			e.KafkaSettings = common.OrElse(in.KafkaSettings, e.KafkaSettings)
			e.ElasticsearchSettings = common.OrElse(in.ElasticsearchSettings, e.ElasticsearchSettings)
			e.NeptuneSettings = common.OrElse(in.NeptuneSettings, e.NeptuneSettings)
			e.RedshiftSettings = common.OrElse(in.RedshiftSettings, e.RedshiftSettings)
			e.PostgreSQLSettings = common.OrElse(in.PostgreSQLSettings, e.PostgreSQLSettings)
			e.MySQLSettings = common.OrElse(in.MySQLSettings, e.MySQLSettings)
			e.OracleSettings = common.OrElse(in.OracleSettings, e.OracleSettings)
			e.SybaseSettings = common.OrElse(in.SybaseSettings, e.SybaseSettings)
			e.MicrosoftSQLServerSettings = common.OrElse(in.MicrosoftSQLServerSettings, e.MicrosoftSQLServerSettings)
			e.IBMDb2Settings = common.OrElse(in.IBMDb2Settings, e.IBMDb2Settings)
			e.DocDbSettings = common.OrElse(in.DocDbSettings, e.DocDbSettings)
			e.RedisSettings = common.OrElse(in.RedisSettings, e.RedisSettings)
			e.GcpMySQLSettings = common.OrElse(in.GcpMySQLSettings, e.GcpMySQLSettings)
			e.TimestreamSettings = common.OrElse(in.TimestreamSettings, e.TimestreamSettings)
			r.Name = common.Value(e.EndpointIdentifier)
			return checkEndpoint(e)
		})
	if err != nil {
		return nil, err
	}

	return &api.ModifyEndpointResponse{Endpoint: r.Endpoint}, nil
}

func (s *Service) DeleteEndpoint(ctx context.Context, in *api.DeleteEndpointMessage,
) (*api.DeleteEndpointResponse, error) {
	r, err := s.remove(ctx, kindEndpoint, common.Value(in.EndpointArn))
	if err != nil {
		return nil, err
	}

	r.Endpoint.Status = new("deleting")
	return &api.DeleteEndpointResponse{Endpoint: r.Endpoint}, nil
}

// redactEndpoint discards all Smithy-sensitive endpoint settings before records or responses exist.
func redactEndpoint(e *api.Endpoint) {
	if e.MongoDbSettings != nil {
		e.MongoDbSettings.Password = nil
	}

	if e.KafkaSettings != nil {
		e.KafkaSettings.SslClientKeyPassword = nil
		e.KafkaSettings.SaslPassword = nil
	}

	if e.RedshiftSettings != nil {
		e.RedshiftSettings.Password = nil
	}

	if e.PostgreSQLSettings != nil {
		e.PostgreSQLSettings.Password = nil
	}

	if e.MySQLSettings != nil {
		e.MySQLSettings.Password = nil
	}

	if e.OracleSettings != nil {
		e.OracleSettings.AsmPassword = nil
		e.OracleSettings.Password = nil
		e.OracleSettings.SecurityDbEncryption = nil
	}

	if e.SybaseSettings != nil {
		e.SybaseSettings.Password = nil
	}

	if e.MicrosoftSQLServerSettings != nil {
		e.MicrosoftSQLServerSettings.Password = nil
	}

	if e.IBMDb2Settings != nil {
		e.IBMDb2Settings.Password = nil
	}

	if e.DocDbSettings != nil {
		e.DocDbSettings.Password = nil
	}

	if e.RedisSettings != nil {
		e.RedisSettings.AuthPassword = nil
	}

	if e.GcpMySQLSettings != nil {
		e.GcpMySQLSettings.Password = nil
	}
}
