package bedrockagent

import (
	"context"
	"strings"

	"gopkg.in/yaml.v3"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockagent"
	"github.com/amjadjibon/tinystack/internal/common"
)

func (s *Service) validateAgent(
	ctx context.Context,
	role, model, key *string,
	guardrail *api.GuardrailConfiguration,
	collaboration *string,
	prompts *api.PromptOverrideConfiguration,
) error {
	if common.Value(collaboration) != "" && common.Value(collaboration) != "DISABLED" {
		return invalid("Agent collaboration is not supported.")
	}

	if checkErr1 := s.requireRole(ctx, role); checkErr1 != nil {
		return checkErr1
	}

	if _, checkErr2 := s.control.Resolve(ctx, common.Value(model)); checkErr2 != nil {
		return invalid("Invalid foundationModel: %v", checkErr2)
	}

	if key != nil {
		if checkErr3 := s.reference(ctx, "kms", "DescribeKey", map[string]any{"KeyId": *key}); checkErr3 != nil {
			return checkErr3
		}
	}

	if guardrail != nil {
		if checkErr4 := s.reference(
			ctx,
			"bedrock",
			"GetGuardrail",
			map[string]any{
				"GuardrailIdentifier": common.Value(guardrail.GuardrailIdentifier),
				"GuardrailVersion":    common.Value(guardrail.GuardrailVersion),
			},
		); checkErr4 != nil {
			return checkErr4
		}
	}

	if prompts != nil && prompts.OverrideLambda != nil {
		if checkErr5 := s.reference(
			ctx,
			"lambda",
			"GetFunction",
			map[string]any{"FunctionName": *prompts.OverrideLambda},
		); checkErr5 != nil {
			return checkErr5
		}
	}

	return nil
}

func (s *Service) validateGroup(
	ctx context.Context,
	executor *api.ActionGroupExecutor,
	schema *api.APISchema,
	functions *api.FunctionSchema,
	parent, description *string,
) error {
	if parent != nil {
		if *parent != "AMAZON.UserInput" && *parent != "AMAZON.CodeInterpreter" {
			return invalid("Unsupported parent action group signature.")
		}

		if executor != nil || schema != nil || functions != nil || description != nil {
			return invalid("Built-in action groups cannot specify executor, schema, or description.")
		}

		return nil
	}

	if executor == nil || (executor.Lambda == nil) == (executor.CustomControl == nil) {
		return invalid("Specify exactly one action group executor.")
	}

	if (schema == nil) == (functions == nil) {
		return invalid("Specify exactly one of apiSchema or functionSchema.")
	}

	if executor.Lambda != nil {
		if checkErr6 := s.reference(
			ctx,
			"lambda",
			"GetFunction",
			map[string]any{"FunctionName": *executor.Lambda},
		); checkErr6 != nil {
			return checkErr6
		}
	}

	if schema != nil {
		if (schema.Payload == nil) == (schema.S3 == nil) {
			return invalid("Specify exactly one API schema source.")
		}

		payload := common.Value(schema.Payload)
		if schema.S3 != nil {
			if s.caller == nil {
				return invalid("Reference validation is unavailable.")
			}

			out, err := s.caller.Call(
				ctx,
				"s3",
				"GetObject",
				map[string]any{
					"Bucket": common.Value(schema.S3.S3BucketName),
					"Key":    common.Value(schema.S3.S3ObjectKey),
				},
			)
			if err != nil {
				return invalid("Invalid schema S3 object: %v", err)
			}

			payload, _ = out["Body"].(string)
		}

		var document map[string]any
		if checkErr7 := yaml.Unmarshal([]byte(payload), &document); checkErr7 != nil {
			return invalid("API schema must be OpenAPI 3 JSON or YAML: %v", checkErr7)
		}

		version, _ := document["openapi"].(string)
		info, infoOK := document["info"].(map[string]any)
		_, pathsOK := document["paths"].(map[string]any)
		title, _ := info["title"].(string)
		infoVersion, _ := info["version"].(string)
		if !strings.HasPrefix(version, "3.") || !infoOK || !pathsOK || title == "" || infoVersion == "" {
			return invalid("API schema must be an OpenAPI 3 document.")
		}
	}

	return nil
}

func (s *Service) validateKnowledgeBase(
	ctx context.Context,
	role *string,
	config *api.KnowledgeBaseConfiguration,
	store *api.StorageConfiguration,
) error {
	if config == nil || common.Value(config.Type) != "VECTOR" || config.VectorKnowledgeBaseConfiguration == nil {
		return invalid("Only VECTOR knowledge bases are supported.")
	}

	if checkErr8 := s.requireRole(ctx, role); checkErr8 != nil {
		return checkErr8
	}

	if store == nil {
		return invalid("storageConfiguration is required for VECTOR knowledge bases.")
	}

	model, err := s.control.Resolve(ctx, common.Value(config.VectorKnowledgeBaseConfiguration.EmbeddingModelArn))
	if err != nil {
		return invalid("Invalid embedding model: %v", err)
	}

	if model.Dimension == 0 {
		return invalid("Knowledge bases require an embedding model.")
	}

	count := 0
	for _, present := range []bool{
		store.OpensearchServerlessConfiguration != nil,
		store.PineconeConfiguration != nil,
		store.RedisEnterpriseCloudConfiguration != nil,
		store.RdsConfiguration != nil,
		store.MongoDbAtlasConfiguration != nil,
		store.NeptuneAnalyticsConfiguration != nil,
		store.S3VectorsConfiguration != nil,
	} {
		if present {
			count++
		}
	}

	if count != 1 {
		return invalid("Specify exactly one storage configuration matching its type.")
	}

	var secret *string
	switch common.Value(store.Type) {
	case "PINECONE":
		if store.PineconeConfiguration != nil {
			secret = store.PineconeConfiguration.CredentialsSecretArn
		}
	case "REDIS_ENTERPRISE_CLOUD":
		if store.RedisEnterpriseCloudConfiguration != nil {
			secret = store.RedisEnterpriseCloudConfiguration.CredentialsSecretArn
		}
	case "MONGO_DB_ATLAS":
		if store.MongoDbAtlasConfiguration != nil {
			secret = store.MongoDbAtlasConfiguration.CredentialsSecretArn
		}
	case "RDS":
		if store.RdsConfiguration != nil {
			secret = store.RdsConfiguration.CredentialsSecretArn
		}
	}

	if secret != nil {
		if secretErr := s.reference(
			ctx,
			"secretsmanager",
			"DescribeSecret",
			map[string]any{"SecretId": *secret},
		); secretErr != nil {
			return secretErr
		}
	}

	switch common.Value(store.Type) {
	case "OPENSEARCH_SERVERLESS":
		if store.OpensearchServerlessConfiguration == nil {
			return invalid("opensearchServerlessConfiguration is required.")
		}

		value := common.Value(store.OpensearchServerlessConfiguration.CollectionArn)
		id := value[strings.LastIndex(value, "/")+1:]
		if s.caller == nil {
			return invalid("Reference validation is unavailable.")
		}

		out, err := s.caller.Call(
			ctx,
			"opensearchserverless",
			"BatchGetCollection",
			map[string]any{"Ids": []any{id}},
		)
		if err != nil {
			return invalid("Invalid collection: %v", err)
		}

		details, _ := out["CollectionDetails"].([]any)
		if len(details) == 0 {
			return invalid("OpenSearch Serverless collection does not exist.")
		}
	case "RDS":
		if store.RdsConfiguration == nil {
			return invalid("rdsConfiguration is required.")
		}

		value := common.Value(store.RdsConfiguration.ResourceArn)
		id := value[strings.LastIndex(value, ":")+1:]
		if checkErr9 := s.reference(
			ctx,
			"rds",
			"DescribeDBClusters",
			map[string]any{"DBClusterIdentifier": id},
		); checkErr9 != nil {
			return checkErr9
		}
	case "PINECONE":
		if store.PineconeConfiguration == nil {
			return invalid("pineconeConfiguration is required.")
		}
	case "REDIS_ENTERPRISE_CLOUD":
		if store.RedisEnterpriseCloudConfiguration == nil {
			return invalid("redisEnterpriseCloudConfiguration is required.")
		}
	case "MONGO_DB_ATLAS":
		if store.MongoDbAtlasConfiguration == nil {
			return invalid("mongoDbAtlasConfiguration is required.")
		}
	case "NEPTUNE_ANALYTICS":
		if store.NeptuneAnalyticsConfiguration == nil {
			return invalid("neptuneAnalyticsConfiguration is required.")
		}
	case "S3_VECTORS":
		if store.S3VectorsConfiguration == nil {
			return invalid("s3VectorsConfiguration is required.")
		}
	default:
		return invalid("Storage type is not supported.")
	}

	return nil
}

func (s *Service) validateDataSource(
	ctx context.Context,
	config *api.DataSourceConfiguration,
	encryption *api.ServerSideEncryptionConfiguration,
) error {
	if config == nil || common.Value(config.Type) != "S3" || config.S3Configuration == nil {
		return invalid("Only S3 data sources are supported.")
	}

	bucket := strings.TrimPrefix(common.Value(config.S3Configuration.BucketArn), "arn:aws:s3:::")
	if checkErr10 := s.reference(ctx, "s3", "HeadBucket", map[string]any{"Bucket": bucket}); checkErr10 != nil {
		return checkErr10
	}

	if encryption != nil && encryption.KmsKeyArn != nil {
		if checkErr11 := s.reference(
			ctx,
			"kms",
			"DescribeKey",
			map[string]any{"KeyId": *encryption.KmsKeyArn},
		); checkErr11 != nil {
			return checkErr11
		}
	}

	return nil
}

func (s *Service) requireKnowledgeBase(ctx context.Context, id *string) error {
	_, err := s.lookup(ctx, kindKnowledgeBase, common.Value(id))
	return err
}

func (s *Service) requireRole(ctx context.Context, role *string) error {
	if s.caller == nil {
		return invalid("Reference validation is unavailable.")
	}

	out, err := s.caller.Call(ctx, "iam", "GetRole", map[string]any{"RoleName": roleName(common.Value(role))})
	if err != nil {
		return invalid("Invalid IAM role: %v", err)
	}

	value, _ := out["Role"].(map[string]any)
	if value["Arn"] != common.Value(role) {
		return invalid("IAM role ARN does not match an existing role.")
	}

	return nil
}
