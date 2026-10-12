module github.com/amjadjibon/tinystack

go 1.27

require (
	github.com/aws/aws-sdk-go-v2 v1.47.3
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.20.12
	github.com/aws/aws-sdk-go-v2/service/account v1.42.3
	github.com/aws/aws-sdk-go-v2/service/acm v1.50.1
	github.com/aws/aws-sdk-go-v2/service/apigateway v1.50.0
	github.com/aws/aws-sdk-go-v2/service/apigatewayv2 v1.44.0
	github.com/aws/aws-sdk-go-v2/service/appconfig v1.54.2
	github.com/aws/aws-sdk-go-v2/service/appconfigdata v1.32.2
	github.com/aws/aws-sdk-go-v2/service/applicationautoscaling v1.43.0
	github.com/aws/aws-sdk-go-v2/service/appsync v1.62.1
	github.com/aws/aws-sdk-go-v2/service/athena v1.66.1
	github.com/aws/aws-sdk-go-v2/service/autoscaling v1.78.1
	github.com/aws/aws-sdk-go-v2/service/backup v1.69.1
	github.com/aws/aws-sdk-go-v2/service/batch v1.78.1
	github.com/aws/aws-sdk-go-v2/service/cloudcontrol v1.38.2
	github.com/aws/aws-sdk-go-v2/service/cloudformation v1.81.1
	github.com/aws/aws-sdk-go-v2/service/cloudfront v1.74.0
	github.com/aws/aws-sdk-go-v2/service/cloudtrail v1.65.2
	github.com/aws/aws-sdk-go-v2/service/cloudwatch v1.73.0
	github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs v1.89.0
	github.com/aws/aws-sdk-go-v2/service/codebuild v1.68.1
	github.com/aws/aws-sdk-go-v2/service/cognitoidentity v1.42.1
	github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider v1.75.0
	github.com/aws/aws-sdk-go-v2/service/configservice v1.74.3
	github.com/aws/aws-sdk-go-v2/service/dynamodb v1.70.0
	github.com/aws/aws-sdk-go-v2/service/dynamodbstreams v1.43.0
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.338.1
	github.com/aws/aws-sdk-go-v2/service/ecr v1.66.1
	github.com/aws/aws-sdk-go-v2/service/ecs v1.100.0
	github.com/aws/aws-sdk-go-v2/service/efs v1.49.4
	github.com/aws/aws-sdk-go-v2/service/eks v1.102.0
	github.com/aws/aws-sdk-go-v2/service/elasticache v1.63.0
	github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2 v1.63.1
	github.com/aws/aws-sdk-go-v2/service/eventbridge v1.55.0
	github.com/aws/aws-sdk-go-v2/service/firehose v1.52.1
	github.com/aws/aws-sdk-go-v2/service/glue v1.167.0
	github.com/aws/aws-sdk-go-v2/service/iam v1.64.1
	github.com/aws/aws-sdk-go-v2/service/iot v1.84.1
	github.com/aws/aws-sdk-go-v2/service/iotdataplane v1.40.1
	github.com/aws/aws-sdk-go-v2/service/iotjobsdataplane v1.38.2
	github.com/aws/aws-sdk-go-v2/service/kafka v1.45.2
	github.com/aws/aws-sdk-go-v2/service/kinesis v1.56.1
	github.com/aws/aws-sdk-go-v2/service/kms v1.61.1
	github.com/aws/aws-sdk-go-v2/service/lambda v1.110.0
	github.com/aws/aws-sdk-go-v2/service/location v1.59.2
	github.com/aws/aws-sdk-go-v2/service/mq v1.36.2
	github.com/aws/aws-sdk-go-v2/service/mwaa v1.48.4
	github.com/aws/aws-sdk-go-v2/service/organizations v1.61.1
	github.com/aws/aws-sdk-go-v2/service/pipes v1.32.2
	github.com/aws/aws-sdk-go-v2/service/rds v1.116.0
	github.com/aws/aws-sdk-go-v2/service/redshift v1.55.0
	github.com/aws/aws-sdk-go-v2/service/resourcegroups v1.42.2
	github.com/aws/aws-sdk-go-v2/service/resourcegroupstaggingapi v1.41.2
	github.com/aws/aws-sdk-go-v2/service/route53 v1.70.1
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.0
	github.com/aws/aws-sdk-go-v2/service/s3tables v1.11.0
	github.com/aws/aws-sdk-go-v2/service/scheduler v1.25.1
	github.com/aws/aws-sdk-go-v2/service/secretsmanager v1.50.1
	github.com/aws/aws-sdk-go-v2/service/servicediscovery v1.49.2
	github.com/aws/aws-sdk-go-v2/service/ses v1.42.2
	github.com/aws/aws-sdk-go-v2/service/sesv2 v1.77.0
	github.com/aws/aws-sdk-go-v2/service/sfn v1.51.1
	github.com/aws/aws-sdk-go-v2/service/sns v1.47.2
	github.com/aws/aws-sdk-go-v2/service/sqs v1.52.1
	github.com/aws/aws-sdk-go-v2/service/ssm v1.79.0
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.1
	github.com/aws/aws-sdk-go-v2/service/transfer v1.85.0
	github.com/aws/aws-sdk-go-v2/service/wafv2 v1.71.1
	github.com/aws/aws-sdk-go-v2/service/xray v1.45.4
	github.com/aws/smithy-go v1.28.5
	github.com/coder/websocket v1.8.15
	github.com/dop251/goja v0.0.0-20261004200024-481fdb442bb4
	github.com/evanphx/json-patch/v5 v5.9.11
	github.com/lib/pq v1.10.9
	github.com/mochi-mqtt/server/v2 v2.7.9
	github.com/recolabs/gnata v0.5.2
	github.com/spf13/cobra v1.10.2
	github.com/vektah/gqlparser/v2 v2.5.60
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.60.1
)

require (
	dario.cat/mergo v1.0.2 // indirect
	github.com/agnivade/levenshtein v1.2.1 // indirect
	github.com/air-verse/air v1.67.4 // indirect
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.20 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.6 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.6 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.5 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/endpoint-discovery v1.13.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.4 // indirect
	github.com/bep/godartsass/v2 v2.5.0 // indirect
	github.com/bep/golibsass v1.2.0 // indirect
	github.com/bits-and-blooms/bitset v1.24.5 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2/v2 v2.8.3 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/gohugoio/hashstructure v0.6.0 // indirect
	github.com/gohugoio/hugo v0.164.0 // indirect
	github.com/google/pprof v0.0.0-20261005154351-639476b4d215 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/joho/godotenv v1.5.1 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.1.0 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rs/xid v1.4.0 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/tdewolff/parse/v2 v2.8.12 // indirect
	github.com/tidwall/gjson v1.20.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.2 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.77.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

tool github.com/air-verse/air
