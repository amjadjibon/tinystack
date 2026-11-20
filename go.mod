module github.com/amjadjibon/tinystack

go 1.27

require (
	github.com/aws/aws-sdk-go-v2 v1.48.0
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.27
	github.com/aws/aws-sdk-go-v2/credentials v1.21.3
	github.com/aws/aws-sdk-go-v2/feature/dsql/auth v1.2.12
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.9
	github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager v0.5.11
	github.com/aws/aws-sdk-go-v2/service/accessanalyzer v1.57.12
	github.com/aws/aws-sdk-go-v2/service/account v1.42.11
	github.com/aws/aws-sdk-go-v2/service/acm v1.52.0
	github.com/aws/aws-sdk-go-v2/service/acmpca v1.57.0
	github.com/aws/aws-sdk-go-v2/service/amp v1.55.5
	github.com/aws/aws-sdk-go-v2/service/amplify v1.49.7
	github.com/aws/aws-sdk-go-v2/service/apigateway v1.53.0
	github.com/aws/aws-sdk-go-v2/service/apigatewaymanagementapi v1.41.7
	github.com/aws/aws-sdk-go-v2/service/apigatewayv2 v1.46.9
	github.com/aws/aws-sdk-go-v2/service/appconfig v1.54.12
	github.com/aws/aws-sdk-go-v2/service/appconfigdata v1.32.11
	github.com/aws/aws-sdk-go-v2/service/applicationautoscaling v1.51.12
	github.com/aws/aws-sdk-go-v2/service/apprunner v1.48.12
	github.com/aws/aws-sdk-go-v2/service/appsync v1.62.12
	github.com/aws/aws-sdk-go-v2/service/athena v1.68.0
	github.com/aws/aws-sdk-go-v2/service/autoscaling v1.82.0
	github.com/aws/aws-sdk-go-v2/service/backup v1.70.0
	github.com/aws/aws-sdk-go-v2/service/batch v1.79.1
	github.com/aws/aws-sdk-go-v2/service/bedrock v1.74.7
	github.com/aws/aws-sdk-go-v2/service/bedrockagent v1.70.1
	github.com/aws/aws-sdk-go-v2/service/bedrockagentcore v1.60.0
	github.com/aws/aws-sdk-go-v2/service/bedrockagentcorecontrol v1.83.0
	github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime v1.67.2
	github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.63.13
	github.com/aws/aws-sdk-go-v2/service/budgets v1.53.9
	github.com/aws/aws-sdk-go-v2/service/cloudcontrol v1.38.12
	github.com/aws/aws-sdk-go-v2/service/cloudformation v1.86.0
	github.com/aws/aws-sdk-go-v2/service/cloudfront v1.76.0
	github.com/aws/aws-sdk-go-v2/service/cloudfrontkeyvaluestore v1.22.6
	github.com/aws/aws-sdk-go-v2/service/cloudtrail v1.65.12
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.76.0
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.92.2
	github.com/aws/aws-sdk-go-v2/service/codeartifact v1.48.6
	github.com/aws/aws-sdk-go-v2/service/codebuild v1.79.0
	github.com/aws/aws-sdk-go-v2/service/codecommit v1.44.7
	github.com/aws/aws-sdk-go-v2/service/codeconnections v1.22.0
	github.com/aws/aws-sdk-go-v2/service/codedeploy v1.47.0
	github.com/aws/aws-sdk-go-v2/service/codepipeline v1.57.0
	github.com/aws/aws-sdk-go-v2/service/cognitoidentity v1.42.12
	github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider v1.80.0
	github.com/aws/aws-sdk-go-v2/service/comprehend v1.50.0
	github.com/aws/aws-sdk-go-v2/service/configservice v1.76.0
	github.com/aws/aws-sdk-go-v2/service/costandusagereportservice v1.43.12
	github.com/aws/aws-sdk-go-v2/service/costexplorer v1.75.0
	github.com/aws/aws-sdk-go-v2/service/databasemigrationservice v1.75.9
	github.com/aws/aws-sdk-go-v2/service/datasync v1.70.0
	github.com/aws/aws-sdk-go-v2/service/docdb v1.59.1
	github.com/aws/aws-sdk-go-v2/service/dsql v1.23.0
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.74.1
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.48.0
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.349.0
	github.com/aws/aws-sdk-go-v2/service/ecr v1.69.0
	github.com/aws/aws-sdk-go-v2/service/ecs v1.106.0
	github.com/aws/aws-sdk-go-v2/service/efs v1.51.6
	github.com/aws/aws-sdk-go-v2/service/eks v1.105.6
	github.com/aws/aws-sdk-go-v2/service/elasticache v1.66.0
	github.com/aws/aws-sdk-go-v2/service/elasticbeanstalk v1.45.0
	github.com/aws/aws-sdk-go-v2/service/elasticloadbalancing v1.42.0
	github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2 v1.65.0
	github.com/aws/aws-sdk-go-v2/service/emr v1.71.3
	github.com/aws/aws-sdk-go-v2/service/emrserverless v1.53.0
	github.com/aws/aws-sdk-go-v2/service/eventbridge v1.55.11
	github.com/aws/aws-sdk-go-v2/service/firehose v1.52.12
	github.com/aws/aws-sdk-go-v2/service/fis v1.46.12
	github.com/aws/aws-sdk-go-v2/service/glacier v1.41.12
	github.com/aws/aws-sdk-go-v2/service/globalaccelerator v1.45.11
	github.com/aws/aws-sdk-go-v2/service/glue v1.176.0
	github.com/aws/aws-sdk-go-v2/service/iam v1.68.0
	github.com/aws/aws-sdk-go-v2/service/identitystore v1.50.0
	github.com/aws/aws-sdk-go-v2/service/inspector2 v1.64.0
	github.com/aws/aws-sdk-go-v2/service/iot v1.84.12
	github.com/aws/aws-sdk-go-v2/service/iotdataplane v1.42.6
	github.com/aws/aws-sdk-go-v2/service/iotjobsdataplane v1.40.6
	github.com/aws/aws-sdk-go-v2/service/iotwireless v1.67.6
	github.com/aws/aws-sdk-go-v2/service/kafka v1.66.0
	github.com/aws/aws-sdk-go-v2/service/keyspaces v1.37.9
	github.com/aws/aws-sdk-go-v2/service/kinesis v1.58.0
	github.com/aws/aws-sdk-go-v2/service/kinesisanalyticsv2 v1.48.0
	github.com/aws/aws-sdk-go-v2/service/kms v1.63.0
	github.com/aws/aws-sdk-go-v2/service/lakeformation v1.59.0
	github.com/aws/aws-sdk-go-v2/service/lambda v1.118.0
	github.com/aws/aws-sdk-go-v2/service/location v1.60.5
	github.com/aws/aws-sdk-go-v2/service/mediaconnect v1.64.0
	github.com/aws/aws-sdk-go-v2/service/mediaconvert v1.108.1
	github.com/aws/aws-sdk-go-v2/service/memorydb v1.43.0
	github.com/aws/aws-sdk-go-v2/service/mq v1.45.12
	github.com/aws/aws-sdk-go-v2/service/mwaa v1.50.0
	github.com/aws/aws-sdk-go-v2/service/neptune v1.55.0
	github.com/aws/aws-sdk-go-v2/service/networkfirewall v1.81.0
	github.com/aws/aws-sdk-go-v2/service/opensearch v1.88.1
	github.com/aws/aws-sdk-go-v2/service/opensearchserverless v1.41.7
	github.com/aws/aws-sdk-go-v2/service/organizations v1.63.0
	github.com/aws/aws-sdk-go-v2/service/pipes v1.32.12
	github.com/aws/aws-sdk-go-v2/service/polly v1.69.0
	github.com/aws/aws-sdk-go-v2/service/ram v1.46.1
	github.com/aws/aws-sdk-go-v2/service/rds v1.134.0
	github.com/aws/aws-sdk-go-v2/service/rdsdata v1.41.5
	github.com/aws/aws-sdk-go-v2/service/redshift v1.72.0
	github.com/aws/aws-sdk-go-v2/service/redshiftdata v1.52.6
	github.com/aws/aws-sdk-go-v2/service/redshiftserverless v1.46.6
	github.com/aws/aws-sdk-go-v2/service/rekognition v1.62.6
	github.com/aws/aws-sdk-go-v2/service/resourceexplorer2 v1.33.12
	github.com/aws/aws-sdk-go-v2/service/resourcegroups v1.42.12
	github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi v1.42.0
	github.com/aws/aws-sdk-go-v2/service/route53 v1.72.0
	github.com/aws/aws-sdk-go-v2/service/route53resolver v1.55.0
	github.com/aws/aws-sdk-go-v2/service/s3 v1.116.0
	github.com/aws/aws-sdk-go-v2/service/s3control v1.81.1
	github.com/aws/aws-sdk-go-v2/service/s3files v1.10.5
	github.com/aws/aws-sdk-go-v2/service/s3tables v1.27.0
	github.com/aws/aws-sdk-go-v2/service/scheduler v1.27.0
	github.com/aws/aws-sdk-go-v2/service/secretsmanager v1.51.0
	github.com/aws/aws-sdk-go-v2/service/securityhub v1.88.0
	github.com/aws/aws-sdk-go-v2/service/servicecatalog v1.50.0
	github.com/aws/aws-sdk-go-v2/service/servicediscovery v1.50.0
	github.com/aws/aws-sdk-go-v2/service/servicequotas v1.43.12
	github.com/aws/aws-sdk-go-v2/service/ses v1.43.0
	github.com/aws/aws-sdk-go-v2/service/sesv2 v1.80.0
	github.com/aws/aws-sdk-go-v2/service/sfn v1.53.0
	github.com/aws/aws-sdk-go-v2/service/shield v1.44.0
	github.com/aws/aws-sdk-go-v2/service/signer v1.41.12
	github.com/aws/aws-sdk-go-v2/service/sns v1.48.0
	github.com/aws/aws-sdk-go-v2/service/sqs v1.53.0
	github.com/aws/aws-sdk-go-v2/service/ssm v1.83.0
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.5
	github.com/aws/aws-sdk-go-v2/service/ssoadmin v1.53.0
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.5
	github.com/aws/aws-sdk-go-v2/service/sts v1.53.0
	github.com/aws/aws-sdk-go-v2/service/swf v1.44.0
	github.com/aws/aws-sdk-go-v2/service/textract v1.50.0
	github.com/aws/aws-sdk-go-v2/service/timestreamquery v1.46.6
	github.com/aws/aws-sdk-go-v2/service/timestreamwrite v1.45.6
	github.com/aws/aws-sdk-go-v2/service/transcribe v1.67.0
	github.com/aws/aws-sdk-go-v2/service/transfer v1.86.8
	github.com/aws/aws-sdk-go-v2/service/translate v1.43.9
	github.com/aws/aws-sdk-go-v2/service/verifiedpermissions v1.44.1
	github.com/aws/aws-sdk-go-v2/service/waf v1.40.6
	github.com/aws/aws-sdk-go-v2/service/wafv2 v1.86.0
	github.com/aws/aws-sdk-go-v2/service/xray v1.46.7
	github.com/aws/smithy-go v1.28.9
	github.com/coder/websocket v1.8.15
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/dop251/goja v0.0.0-20261027234916-5f913d1693d9
	github.com/evanphx/json-patch/v5 v5.9.11
	github.com/go-sql-driver/mysql v1.10.1
	github.com/google/jsonschema-go v0.4.3
	github.com/lib/pq v1.12.3
	github.com/miekg/dns v1.1.73
	github.com/mochi-mqtt/server/v2 v2.7.9
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/parquet-go/parquet-go v0.32.0
	github.com/pkg/sftp v1.13.12
	github.com/rabbitmq/amqp091-go v1.16.0
	github.com/recolabs/gnata v0.6.2
	github.com/redis/go-redis/v9 v9.23.0
	github.com/spf13/cobra v1.10.2
	github.com/twmb/franz-go v1.22.2
	github.com/twmb/franz-go/pkg/kfake v0.0.0-20261019220705-4f5e2c0e3950
	github.com/twmb/franz-go/pkg/kmsg v1.15.0
	github.com/vektah/gqlparser/v2 v2.5.64
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sys v0.49.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.61.0
)

require (
	dario.cat/mergo v1.0.2 // indirect
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/agnivade/levenshtein v1.2.2 // indirect
	github.com/air-verse/air v1.67.4 // indirect
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.12 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.25 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.13 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.13.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.12 // indirect
	github.com/bep/godartsass/v2 v2.5.0 // indirect
	github.com/bep/golibsass v1.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2/v2 v2.11.0 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/gohugoio/hashstructure v0.6.0 // indirect
	github.com/gohugoio/hugo v0.164.0 // indirect
	github.com/google/pprof v0.0.0-20261016150608-1facf8f91b50 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/parquet-go/bitpack v1.0.0 // indirect
	github.com/parquet-go/jsonlite v1.0.0 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.33 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.4.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tdewolff/parse/v2 v2.8.12 // indirect
	github.com/tidwall/gjson v1.20.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.2 // indirect
	github.com/twpayne/go-geom v1.6.1 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.79.0 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

tool github.com/air-verse/air
