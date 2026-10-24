// Package app assembles TinyStack: every service over one store, wired to each
// other and to the gateway. The start command and the integration tests both
// build it here, so tests run the wiring that ships.
package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	wiredns "github.com/miekg/dns"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	accessanalyzerapi "github.com/amjadjibon/tinystack/internal/aws/generated/accessanalyzer"
	accountapi "github.com/amjadjibon/tinystack/internal/aws/generated/account"
	acmapi "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	acmpcaapi "github.com/amjadjibon/tinystack/internal/aws/generated/acmpca"
	ampapi "github.com/amjadjibon/tinystack/internal/aws/generated/amp"
	amplifyapi "github.com/amjadjibon/tinystack/internal/aws/generated/amplify"
	apigatewayapi "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	apigatewaymanagementapiapi "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewaymanagementapi"
	apigatewayv2api "github.com/amjadjibon/tinystack/internal/aws/generated/apigatewayv2"
	appconfigapi "github.com/amjadjibon/tinystack/internal/aws/generated/appconfig"
	appconfigdataapi "github.com/amjadjibon/tinystack/internal/aws/generated/appconfigdata"
	applicationautoscalingapi "github.com/amjadjibon/tinystack/internal/aws/generated/applicationautoscaling"
	apprunnerapi "github.com/amjadjibon/tinystack/internal/aws/generated/apprunner"
	appsyncapi "github.com/amjadjibon/tinystack/internal/aws/generated/appsync"
	athenaapi "github.com/amjadjibon/tinystack/internal/aws/generated/athena"
	autoscalingapi "github.com/amjadjibon/tinystack/internal/aws/generated/autoscaling"
	backupapi "github.com/amjadjibon/tinystack/internal/aws/generated/backup"
	batchapi "github.com/amjadjibon/tinystack/internal/aws/generated/batch"
	bedrockapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrock"
	bedrockruntimeapi "github.com/amjadjibon/tinystack/internal/aws/generated/bedrockruntime"
	budgetsapi "github.com/amjadjibon/tinystack/internal/aws/generated/budgets"
	cloudcontrolapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudcontrol"
	cloudformationapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudformation"
	cloudfrontapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudfront"
	cloudtrailapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudtrail"
	cloudwatchapi "github.com/amjadjibon/tinystack/internal/aws/generated/cloudwatch"
	codeartifactapi "github.com/amjadjibon/tinystack/internal/aws/generated/codeartifact"
	codebuildapi "github.com/amjadjibon/tinystack/internal/aws/generated/codebuild"
	codecommitapi "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	codeconnectionsapi "github.com/amjadjibon/tinystack/internal/aws/generated/codeconnections"
	codedeployapi "github.com/amjadjibon/tinystack/internal/aws/generated/codedeploy"
	codepipelineapi "github.com/amjadjibon/tinystack/internal/aws/generated/codepipeline"
	cognitoidentityapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidentity"
	cognitoidpapi "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	configapi "github.com/amjadjibon/tinystack/internal/aws/generated/config"
	costandusagereportserviceapi "github.com/amjadjibon/tinystack/internal/aws/generated/costandusagereportservice"
	costexplorerapi "github.com/amjadjibon/tinystack/internal/aws/generated/costexplorer"
	databasemigrationserviceapi "github.com/amjadjibon/tinystack/internal/aws/generated/databasemigrationservice"
	datasyncapi "github.com/amjadjibon/tinystack/internal/aws/generated/datasync"
	dynamodbapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodb"
	dynamodbstreamsapi "github.com/amjadjibon/tinystack/internal/aws/generated/dynamodbstreams"
	ec2api "github.com/amjadjibon/tinystack/internal/aws/generated/ec2"
	ecrapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	ecsapi "github.com/amjadjibon/tinystack/internal/aws/generated/ecs"
	efsapi "github.com/amjadjibon/tinystack/internal/aws/generated/efs"
	eksapi "github.com/amjadjibon/tinystack/internal/aws/generated/eks"
	elasticacheapi "github.com/amjadjibon/tinystack/internal/aws/generated/elasticache"
	elasticbeanstalkapi "github.com/amjadjibon/tinystack/internal/aws/generated/elasticbeanstalk"
	elbapi "github.com/amjadjibon/tinystack/internal/aws/generated/elb"
	elbv2api "github.com/amjadjibon/tinystack/internal/aws/generated/elbv2"
	emrapi "github.com/amjadjibon/tinystack/internal/aws/generated/emr"
	emrserverlessapi "github.com/amjadjibon/tinystack/internal/aws/generated/emrserverless"
	eventbridgeapi "github.com/amjadjibon/tinystack/internal/aws/generated/eventbridge"
	firehoseapi "github.com/amjadjibon/tinystack/internal/aws/generated/firehose"
	fisapi "github.com/amjadjibon/tinystack/internal/aws/generated/fis"
	glacierapi "github.com/amjadjibon/tinystack/internal/aws/generated/glacier"
	globalacceleratorapi "github.com/amjadjibon/tinystack/internal/aws/generated/globalaccelerator"
	glueapi "github.com/amjadjibon/tinystack/internal/aws/generated/glue"
	iamapi "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	identitystoreapi "github.com/amjadjibon/tinystack/internal/aws/generated/identitystore"
	inspector2api "github.com/amjadjibon/tinystack/internal/aws/generated/inspector2"
	iotapi "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	iotdataplaneapi "github.com/amjadjibon/tinystack/internal/aws/generated/iotdataplane"
	iotjobsdataplaneapi "github.com/amjadjibon/tinystack/internal/aws/generated/iotjobsdataplane"
	kafkaapi "github.com/amjadjibon/tinystack/internal/aws/generated/kafka"
	kinesisapi "github.com/amjadjibon/tinystack/internal/aws/generated/kinesis"
	kinesisanalyticsv2api "github.com/amjadjibon/tinystack/internal/aws/generated/kinesisanalyticsv2"
	kmsapi "github.com/amjadjibon/tinystack/internal/aws/generated/kms"
	lakeformationapi "github.com/amjadjibon/tinystack/internal/aws/generated/lakeformation"
	lambdaapi "github.com/amjadjibon/tinystack/internal/aws/generated/lambda"
	locationapi "github.com/amjadjibon/tinystack/internal/aws/generated/location"
	logsapi "github.com/amjadjibon/tinystack/internal/aws/generated/logs"
	memorydbapi "github.com/amjadjibon/tinystack/internal/aws/generated/memorydb"
	mqapi "github.com/amjadjibon/tinystack/internal/aws/generated/mq"
	mwaaapi "github.com/amjadjibon/tinystack/internal/aws/generated/mwaa"
	networkfirewallapi "github.com/amjadjibon/tinystack/internal/aws/generated/networkfirewall"
	opensearchapi "github.com/amjadjibon/tinystack/internal/aws/generated/opensearch"
	opensearchserverlessapi "github.com/amjadjibon/tinystack/internal/aws/generated/opensearchserverless"
	organizationsapi "github.com/amjadjibon/tinystack/internal/aws/generated/organizations"
	pipesapi "github.com/amjadjibon/tinystack/internal/aws/generated/pipes"
	ramapi "github.com/amjadjibon/tinystack/internal/aws/generated/ram"
	rdsapi "github.com/amjadjibon/tinystack/internal/aws/generated/rds"
	rdsdataapi "github.com/amjadjibon/tinystack/internal/aws/generated/rdsdata"
	redshiftapi "github.com/amjadjibon/tinystack/internal/aws/generated/redshift"
	resourceexplorer2api "github.com/amjadjibon/tinystack/internal/aws/generated/resourceexplorer2"
	resourcegroupsapi "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroups"
	resourcegroupstaggingapiapi "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroupstaggingapi"
	route53api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	route53resolverapi "github.com/amjadjibon/tinystack/internal/aws/generated/route53resolver"
	s3api "github.com/amjadjibon/tinystack/internal/aws/generated/s3"
	s3tablesapi "github.com/amjadjibon/tinystack/internal/aws/generated/s3tables"
	schedulerapi "github.com/amjadjibon/tinystack/internal/aws/generated/scheduler"
	secretsmanagerapi "github.com/amjadjibon/tinystack/internal/aws/generated/secretsmanager"
	securityhubapi "github.com/amjadjibon/tinystack/internal/aws/generated/securityhub"
	servicecatalogapi "github.com/amjadjibon/tinystack/internal/aws/generated/servicecatalog"
	servicediscoveryapi "github.com/amjadjibon/tinystack/internal/aws/generated/servicediscovery"
	servicequotasapi "github.com/amjadjibon/tinystack/internal/aws/generated/servicequotas"
	sesapi "github.com/amjadjibon/tinystack/internal/aws/generated/ses"
	sesv2api "github.com/amjadjibon/tinystack/internal/aws/generated/sesv2"
	sfnapi "github.com/amjadjibon/tinystack/internal/aws/generated/sfn"
	shieldapi "github.com/amjadjibon/tinystack/internal/aws/generated/shield"
	signerapi "github.com/amjadjibon/tinystack/internal/aws/generated/signer"
	snsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sns"
	sqsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sqs"
	ssmapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssm"
	ssoadminapi "github.com/amjadjibon/tinystack/internal/aws/generated/ssoadmin"
	stsapi "github.com/amjadjibon/tinystack/internal/aws/generated/sts"
	timestreamwriteapi "github.com/amjadjibon/tinystack/internal/aws/generated/timestreamwrite"
	transferapi "github.com/amjadjibon/tinystack/internal/aws/generated/transfer"
	verifiedpermissionsapi "github.com/amjadjibon/tinystack/internal/aws/generated/verifiedpermissions"
	wafapi "github.com/amjadjibon/tinystack/internal/aws/generated/waf"
	wafv2api "github.com/amjadjibon/tinystack/internal/aws/generated/wafv2"
	xrayapi "github.com/amjadjibon/tinystack/internal/aws/generated/xray"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/config"
	"github.com/amjadjibon/tinystack/internal/delivery"
	dnsserver "github.com/amjadjibon/tinystack/internal/dns"
	"github.com/amjadjibon/tinystack/internal/gateway"
	"github.com/amjadjibon/tinystack/internal/inithooks"
	"github.com/amjadjibon/tinystack/internal/metadata"
	"github.com/amjadjibon/tinystack/internal/services/accessanalyzer"
	"github.com/amjadjibon/tinystack/internal/services/account"
	"github.com/amjadjibon/tinystack/internal/services/acm"
	"github.com/amjadjibon/tinystack/internal/services/acmpca"
	"github.com/amjadjibon/tinystack/internal/services/amp"
	"github.com/amjadjibon/tinystack/internal/services/amplify"
	"github.com/amjadjibon/tinystack/internal/services/apigateway"
	"github.com/amjadjibon/tinystack/internal/services/apigateway/proxy"
	"github.com/amjadjibon/tinystack/internal/services/apigatewaymanagementapi"
	"github.com/amjadjibon/tinystack/internal/services/apigatewayv2"
	"github.com/amjadjibon/tinystack/internal/services/appconfig"
	"github.com/amjadjibon/tinystack/internal/services/applicationautoscaling"
	"github.com/amjadjibon/tinystack/internal/services/apprunner"
	"github.com/amjadjibon/tinystack/internal/services/appsync"
	"github.com/amjadjibon/tinystack/internal/services/athena"
	"github.com/amjadjibon/tinystack/internal/services/autoscaling"
	"github.com/amjadjibon/tinystack/internal/services/awsmetrics"
	"github.com/amjadjibon/tinystack/internal/services/backup"
	"github.com/amjadjibon/tinystack/internal/services/batch"
	"github.com/amjadjibon/tinystack/internal/services/bedrock"
	"github.com/amjadjibon/tinystack/internal/services/bedrockruntime"
	"github.com/amjadjibon/tinystack/internal/services/budgets"
	"github.com/amjadjibon/tinystack/internal/services/cloudcontrol"
	"github.com/amjadjibon/tinystack/internal/services/cloudformation"
	"github.com/amjadjibon/tinystack/internal/services/cloudfront"
	"github.com/amjadjibon/tinystack/internal/services/cloudtrail"
	"github.com/amjadjibon/tinystack/internal/services/cloudwatch"
	"github.com/amjadjibon/tinystack/internal/services/codeartifact"
	"github.com/amjadjibon/tinystack/internal/services/codebuild"
	"github.com/amjadjibon/tinystack/internal/services/codecommit"
	"github.com/amjadjibon/tinystack/internal/services/codeconnections"
	"github.com/amjadjibon/tinystack/internal/services/codedeploy"
	"github.com/amjadjibon/tinystack/internal/services/codepipeline"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidentity"
	"github.com/amjadjibon/tinystack/internal/services/cognitoidp"
	configservice "github.com/amjadjibon/tinystack/internal/services/config"
	"github.com/amjadjibon/tinystack/internal/services/costandusagereportservice"
	"github.com/amjadjibon/tinystack/internal/services/costdata"
	"github.com/amjadjibon/tinystack/internal/services/costexplorer"
	"github.com/amjadjibon/tinystack/internal/services/databasemigrationservice"
	"github.com/amjadjibon/tinystack/internal/services/datasync"
	"github.com/amjadjibon/tinystack/internal/services/dynamodb"
	"github.com/amjadjibon/tinystack/internal/services/ec2"
	"github.com/amjadjibon/tinystack/internal/services/ecr"
	"github.com/amjadjibon/tinystack/internal/services/ecs"
	ecsdocker "github.com/amjadjibon/tinystack/internal/services/ecs/docker"
	"github.com/amjadjibon/tinystack/internal/services/efs"
	"github.com/amjadjibon/tinystack/internal/services/eks"
	"github.com/amjadjibon/tinystack/internal/services/eks/k3s"
	"github.com/amjadjibon/tinystack/internal/services/elasticache"
	cachedocker "github.com/amjadjibon/tinystack/internal/services/elasticache/docker"
	"github.com/amjadjibon/tinystack/internal/services/elasticbeanstalk"
	"github.com/amjadjibon/tinystack/internal/services/elb"
	"github.com/amjadjibon/tinystack/internal/services/elbv2"
	"github.com/amjadjibon/tinystack/internal/services/emr"
	"github.com/amjadjibon/tinystack/internal/services/emrserverless"
	"github.com/amjadjibon/tinystack/internal/services/eventbridge"
	"github.com/amjadjibon/tinystack/internal/services/firehose"
	"github.com/amjadjibon/tinystack/internal/services/fis"
	"github.com/amjadjibon/tinystack/internal/services/glacier"
	"github.com/amjadjibon/tinystack/internal/services/globalaccelerator"
	"github.com/amjadjibon/tinystack/internal/services/glue"
	"github.com/amjadjibon/tinystack/internal/services/iam"
	"github.com/amjadjibon/tinystack/internal/services/identitystore"
	"github.com/amjadjibon/tinystack/internal/services/inspector2"
	"github.com/amjadjibon/tinystack/internal/services/iot"
	"github.com/amjadjibon/tinystack/internal/services/kafka"
	"github.com/amjadjibon/tinystack/internal/services/kinesis"
	"github.com/amjadjibon/tinystack/internal/services/kinesisanalyticsv2"
	"github.com/amjadjibon/tinystack/internal/services/kms"
	"github.com/amjadjibon/tinystack/internal/services/lakeformation"
	"github.com/amjadjibon/tinystack/internal/services/lambda"
	"github.com/amjadjibon/tinystack/internal/services/location"
	"github.com/amjadjibon/tinystack/internal/services/logs"
	"github.com/amjadjibon/tinystack/internal/services/memorydb"
	"github.com/amjadjibon/tinystack/internal/services/mq"
	"github.com/amjadjibon/tinystack/internal/services/mwaa"
	"github.com/amjadjibon/tinystack/internal/services/networkfirewall"
	"github.com/amjadjibon/tinystack/internal/services/opensearch"
	"github.com/amjadjibon/tinystack/internal/services/opensearchserverless"
	"github.com/amjadjibon/tinystack/internal/services/organizations"
	"github.com/amjadjibon/tinystack/internal/services/pipes"
	"github.com/amjadjibon/tinystack/internal/services/ram"
	"github.com/amjadjibon/tinystack/internal/services/rds"
	rdsdocker "github.com/amjadjibon/tinystack/internal/services/rds/docker"
	"github.com/amjadjibon/tinystack/internal/services/rdsdata"
	"github.com/amjadjibon/tinystack/internal/services/redshift"
	"github.com/amjadjibon/tinystack/internal/services/resourceexplorer2"
	"github.com/amjadjibon/tinystack/internal/services/resourcegroups"
	"github.com/amjadjibon/tinystack/internal/services/resourcegroupstaggingapi"
	"github.com/amjadjibon/tinystack/internal/services/route53"
	"github.com/amjadjibon/tinystack/internal/services/route53resolver"
	"github.com/amjadjibon/tinystack/internal/services/s3"
	"github.com/amjadjibon/tinystack/internal/services/s3tables"
	"github.com/amjadjibon/tinystack/internal/services/scheduler"
	"github.com/amjadjibon/tinystack/internal/services/secretsmanager"
	"github.com/amjadjibon/tinystack/internal/services/securityhub"
	"github.com/amjadjibon/tinystack/internal/services/servicecatalog"
	"github.com/amjadjibon/tinystack/internal/services/servicediscovery"
	"github.com/amjadjibon/tinystack/internal/services/servicequotas"
	"github.com/amjadjibon/tinystack/internal/services/ses"
	"github.com/amjadjibon/tinystack/internal/services/sesv2"
	"github.com/amjadjibon/tinystack/internal/services/sfn"
	"github.com/amjadjibon/tinystack/internal/services/shield"
	"github.com/amjadjibon/tinystack/internal/services/signer"
	"github.com/amjadjibon/tinystack/internal/services/sns"
	"github.com/amjadjibon/tinystack/internal/services/sqs"
	"github.com/amjadjibon/tinystack/internal/services/ssm"
	"github.com/amjadjibon/tinystack/internal/services/ssoadmin"
	"github.com/amjadjibon/tinystack/internal/services/sts"
	"github.com/amjadjibon/tinystack/internal/services/timestreamwrite"
	"github.com/amjadjibon/tinystack/internal/services/transfer"
	"github.com/amjadjibon/tinystack/internal/services/verifiedpermissions"
	"github.com/amjadjibon/tinystack/internal/services/waf"
	"github.com/amjadjibon/tinystack/internal/services/wafv2"
	"github.com/amjadjibon/tinystack/internal/services/xray"
	"github.com/amjadjibon/tinystack/internal/sso"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// App is an assembled TinyStack.
type App struct {
	// Gateway serves every service's operations and TinyStack's own
	// /_tinystack/ endpoints.
	Gateway *gateway.Server
	// Metadata serves EC2 IMDS and ECS Task Metadata v4.
	Metadata *metadata.Service
	// IoT accepts MQTT connections on listeners the caller opens.
	IoT *iot.Service
	// Init runs the init scripts and reports how they went. Run runs the
	// boot, start, ready, and shutdown stages; New runs none of them.
	Init *inithooks.Runner
	// Services lists the registered services' names.
	Services []string

	store      storage.Store
	deliveries *delivery.Log
	stops      []func()
	// resets clear in-memory state that a cleared store leaves behind.
	resets  []func(context.Context)
	once    sync.Once
	resetMu sync.Mutex
}

// New builds every service over store, wires them through the gateway, and
// resumes Lambda's event source mappings. runtime runs Lambda functions; the
// configuration picks the ECS, EKS, and ElastiCache runtimes. Stop releases it.
//
// Configuration is passed by value like config.Load returns it. Step Functions and
// Cloud Control set up their own contexts for work that outlives ctx.
//
//nolint:gocritic,contextcheck // See above.
func New(
	ctx context.Context, cfg config.Config, store storage.Store, runtime lambda.Runtime, logger *slog.Logger,
) (*App, error) {
	return build(ctx, cfg, store, runtime, logger, inithooks.New(cfg, logger))
}

// build is New with the runner of init scripts, which Run has already used to
// run the boot stage.
//
//nolint:gocritic,contextcheck // See New.
func build(
	ctx context.Context,
	cfg config.Config,
	store storage.Store,
	runtime lambda.Runtime,
	logger *slog.Logger,
	hooks *inithooks.Runner,
) (a *App, err error) {
	a = &App{store: store, Init: hooks}
	defer func(cleanup *App) {
		if err != nil {
			cleanup.Stop()
		}
	}(a)

	deliveries := delivery.New()
	a.deliveries = deliveries
	tables := dynamodb.New(store)
	// Stop ends the TTL sweeper, which writes to the store.
	a.onStop(tables.Stop)
	keys := kms.New(store)
	queues := sqs.New(store, keys)
	// Stop ends the message mover, which writes to the store.
	a.onStop(queues.Stop)
	logGroups := logs.New(store)
	functions := lambda.New(
		store,
		queues,
		tables.Streams(),
		logGroups,
		runtime,
		logger,
		cfg.Identity.SecretKeys,
		deliveries,
	)
	// Stop removes function containers; it must finish before exit.
	a.onStop(functions.Stop)
	// Reset ends the pollers first, so they do not write what the reset clears.
	a.onReset(functions.Reset)
	a.onReset(queues.Reset)
	streams := kinesis.New(store)
	functions.UseKinesis(streams)
	tables.UseKinesis(streams)
	topics := sns.New(store, queues, functions, deliveries)
	// Stop ends HTTP deliveries in flight.
	a.onStop(topics.Stop)
	secrets := secretsmanager.New(store, keys, functions, deliveries)
	// Stop ends the rotation scheduler and running rotations.
	a.onStop(secrets.Stop)
	rules := eventbridge.New(store, queues, topics, functions, deliveries, secrets)
	// Stop ends the scheduler, which writes to the store.
	a.onStop(rules.Stop)
	blobDir := ""
	if cfg.Storage.Mode == config.StoragePersistent {
		blobDir = cfg.Storage.Path
		if blobDir == "~" || strings.HasPrefix(blobDir, "~/") {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return nil, homeErr
			}

			blobDir = filepath.Join(home, blobDir[1:])
		}
	}

	blobs, blobErr := storage.OpenBlobs(blobDir)
	if blobErr != nil {
		return nil, blobErr
	}

	buckets := s3.New(store, queues, topics, functions, rules, deliveries, blobs)
	a.onStop(buckets.Stop)
	a.onReset(buckets.Reset)
	if resumeErr := buckets.Resume(ctx); resumeErr != nil {
		return nil, resumeErr
	}

	buckets.UseMaxObjectBytes(cfg.Storage.MaxObjectBytes)
	functions.UseObjects(buckets.Object)
	roles := iam.New(store)
	keys.UseAuthorization(roles.Authorization)
	tokens := sts.New(store, roles)
	tokens.UseAccounts(cfg.Identity.Accounts, cfg.Defaults.Account)
	identities := cognitoidp.New(store, functions, deliveries)
	registry := ecr.New(store)
	containers := ecs.New(store)
	containers.UseEndpoint("http://" + net.JoinHostPort("host.docker.internal", strconv.Itoa(cfg.Server.Port)))
	if cfg.ECS.Containers == config.ContainersDocker {
		containers.UseRuntime(ecsdocker.New(), logGroups.Append, logger)
	}

	// Stop ends the task runner and removes task containers.
	a.onStop(containers.Stop)
	a.onReset(containers.Reset)
	httpAPIs := apigatewayv2.New(store, functions, logGroups)
	connectionAPI := apigatewaymanagementapi.New(httpAPIs)
	a.onStop(httpAPIs.Stop)
	a.onReset(httpAPIs.Reset)
	restAPIs := apigateway.New(store, functions, logGroups, identities)
	graphQL := appsync.New(store, functions, identities)
	// Stop closes real-time connections, which shutdown leaves open.
	a.onStop(graphQL.Stop)
	network := ec2.New(store)
	network.UseInstanceProfiles(roles.InstanceProfileOf)
	kube := eks.New(store, network)
	// Stop ends the cluster runner and removes k3s containers.
	a.onStop(kube.Stop)
	a.onReset(kube.Reset)
	workflows, err := sfn.New(store, logGroups, logger)
	if err != nil {
		return nil, err
	}

	// Stop ends running executions.
	a.onStop(workflows.Stop)
	firehoses := firehose.New(store, streams, deliveries, logger)
	// Stop ends the flusher, which writes to the store.
	a.onStop(firehoses.Stop)
	pipelines := pipes.New(store, queues, streams, tables.Streams(), logger)
	// Stop ends the pipe runner, which writes to the store.
	a.onStop(pipelines.Stop)
	a.onReset(pipelines.Reset)
	monitoring := cloudwatch.New(store, logger)
	a.onStop(monitoring.Stop)
	// Reset drops built-in metrics not yet stored, which the reset would otherwise bring back.
	a.onReset(monitoring.Reset)
	// The services above report their AWS/* metrics to CloudWatch, which is built after them.
	for _, service := range []interface{ UseMetrics(awsmetrics.Sink) }{
		functions, queues, tables, restAPIs, httpAPIs, topics, streams,
	} {
		service.UseMetrics(monitoring)
	}

	dns := route53.New(store)
	cdn := cloudfront.New(store)
	timers := scheduler.New(store, logger)
	a.onStop(timers.Stop)
	scaling := autoscaling.New(store, logger)
	a.onStop(scaling.Stop)
	balancers := elbv2.New(store, logger)
	organizationsService := organizations.New(store)
	accounts := account.New(store)
	if err = accounts.SeedFromConfig(ctx, cfg.Accounts); err != nil {
		return nil, err
	}

	discovery := servicediscovery.New(store)
	configurations := appconfig.New(store)
	trails := cloudtrail.New(store, buckets)
	configRecords := configservice.New(store, buckets)
	backupRecords := backup.New(store)
	fileSystems := efs.New(store, network)
	batchRecords := batch.New(store, network)
	buildProjects := codebuild.New(store)
	codePipelines := codepipeline.New(store)
	codecommitRecords := codecommit.New(store)
	artifactRecords := codeartifact.New(store)
	timestreamRecords := timestreamwrite.New(store)
	costExplorerRecords := costexplorer.New(store)
	curRecords := costandusagereportservice.New(store)
	budgetRecords := budgets.New(store, deliveries, logger)
	a.onStop(budgetRecords.Stop)
	appRunnerRecords := apprunner.New(store)
	amplifyRecords := amplify.New(store)
	connectionRecords := codeconnections.New(store)
	ampRecords := amp.New(store)
	resourceExplorerRecords := resourceexplorer2.New(store)
	serviceCatalogRecords := servicecatalog.New(store)
	globalAcceleratorRecords := globalaccelerator.New(store, network, balancers, buckets)
	fisRecords := fis.New(store)
	a.onStop(fisRecords.Stop)
	dataSyncRecords := datasync.New(store, buckets, roles)
	a.onStop(dataSyncRecords.Stop)
	shieldRecords := shield.New(store)
	accessAnalyzerRecords := accessanalyzer.New(store)
	securityHubRecords := securityhub.New(store)
	inspector2Records := inspector2.New(store)
	quotaRecords := servicequotas.New(store)
	dmsRecords := databasemigrationservice.New(store, network)
	emrRecords := emrserverless.New(store)
	beanstalk := elasticbeanstalk.New(store, buckets, logger)
	a.onStop(beanstalk.Stop)
	classicEMR := emr.New(store, network, roles, buckets)
	aossRecords := opensearchserverless.New(store)
	flinkRecords := kinesisanalyticsv2.New(store, network, buckets, logGroups)
	lakeRecords := lakeformation.New(store)
	brokers := mq.New(store, network)
	kafkaClusters := kafka.New(store, network)
	airflowEnvironments := mwaa.New(store, network, buckets)
	transferRecords := transfer.New(store)
	codeDeployRecords := codedeploy.New(store)
	applicationScaling := applicationautoscaling.New(store)
	locationTrackers := location.New(store)
	tableRecords := s3tables.New(store)
	wafClassicRecords := waf.New(store)
	wafRecords := wafv2.New(store)
	networkFirewallRecords := networkfirewall.New(store, network)
	opensearchRecords := opensearch.New(store)
	resolverRecords := route53resolver.New(store, network)
	xrayRecords := xray.New(store)
	glacierRecords := glacier.New(store)
	identityStoreRecords := identitystore.New(store)
	ssoAdminRecords := ssoadmin.New(store)
	ramRecords := ram.New(store)
	privateAuthorities := acmpca.New(store, buckets)
	verifiedpermissionsRecords := verifiedpermissions.New(store)
	redshiftRecords := redshift.New(store, network)
	memoryDBRecords := memorydb.New(store, network)
	classicBalancers := elb.New(store, network)
	a.onReset(trails.Reset)
	databases := rds.New(store)
	dataAPI := rdsdata.New(databases)
	a.onReset(dataAPI.Reset)
	a.onStop(databases.Stop)
	a.onStop(dataAPI.Stop)
	a.onReset(databases.Reset)
	caches := elasticache.New(store)
	a.onStop(caches.Stop)
	a.onReset(caches.Reset)
	things := iot.New(store, deliveries, logger)
	// Stop disconnects every MQTT client.
	a.onStop(things.Stop)
	a.onReset(things.Reset)
	things.UseIdentity(
		request.Scope{AccountID: cfg.Defaults.Account, Region: cfg.Defaults.Region},
		cfg.Identity.Mode == config.IdentityVerify,
	)
	control, err := cloudcontrol.New(store)
	if err != nil {
		return nil, err
	}

	stacks := cloudformation.New(store, logger)
	// Stop ends the stack runner, which writes to the store.
	a.onStop(stacks.Stop)
	email := sesv2.New(store, deliveries)
	identityPools := cognitoidentity.New(store, identities, roles)
	dataCatalog := glue.New(store)
	queries := athena.New(store, dataCatalog, buckets)
	parameters := ssm.New(store, keys)
	// Systems Manager publishes the IDs of EC2's images.
	parameters.UsePublicParameters(ec2.PublicParameters)
	// Run Command targets the running instances EC2 knows.
	parameters.UseInstances(func(ctx context.Context, scope request.Scope) ([]ssm.Instance, error) {
		running, listErr := network.RunningInstances(ctx, scope)
		out := make([]ssm.Instance, len(running))
		for i, r := range running {
			out[i] = ssm.Instance{ID: r.ID, Tags: r.Tags}
		}

		return out, listErr
	})

	// Stop ends the sweeper that expires parameters and finishes commands.
	a.onStop(parameters.Stop)
	certificates := acm.New(store)
	a.onReset(certificates.Reset)
	signing := signer.New(store, buckets, certificates)
	foundationModels := bedrock.New(store)
	inference := bedrockruntime.New(store, foundationModels)
	tags := resourcegroupstaggingapi.New()
	groups := resourcegroups.New(store)
	services := []catalog.Service{
		dynamodbapi.Descriptor(tables),
		dynamodbstreamsapi.Descriptor(tables.Streams()),
		sqsapi.Descriptor(queues),
		snsapi.Descriptor(topics),
		s3api.Descriptor(buckets),
		lambdaapi.Descriptor(functions),
		eventbridgeapi.Descriptor(rules),
		iamapi.Descriptor(roles),
		stsapi.Descriptor(tokens),
		ssmapi.Descriptor(parameters),
		secretsmanagerapi.Descriptor(secrets),
		logsapi.Descriptor(logGroups),
		kmsapi.Descriptor(keys),
		ec2api.Descriptor(network),
		cognitoidp.ProtectProviderCredentials(cognitoidpapi.Descriptor(identities)),
		ecrapi.Descriptor(registry),
		ecsapi.Descriptor(containers),
		eksapi.Descriptor(kube),
		elasticacheapi.Descriptor(caches),
		elasticbeanstalkapi.Descriptor(beanstalk),
		organizationsapi.Descriptor(organizationsService),
		accountapi.Descriptor(accounts),
		servicediscoveryapi.Descriptor(discovery),
		appconfigapi.Descriptor(configurations),
		appconfigdataapi.Descriptor(configurations.Data()),
		cloudtrailapi.Descriptor(trails),
		configapi.Descriptor(configRecords),
		backupapi.Descriptor(backupRecords),
		efsapi.Descriptor(fileSystems),
		batchapi.Descriptor(batchRecords),
		codebuildapi.Descriptor(buildProjects),
		codepipelineapi.Descriptor(codePipelines),
		codecommitapi.Descriptor(codecommitRecords),
		codeartifactapi.Descriptor(artifactRecords),
		timestreamwriteapi.Descriptor(timestreamRecords),
		budgetsapi.Descriptor(budgetRecords),
		apprunnerapi.Descriptor(appRunnerRecords),
		amplifyapi.Descriptor(amplifyRecords),
		codeconnectionsapi.Descriptor(connectionRecords),
		ampapi.Descriptor(ampRecords),
		servicecatalogapi.Descriptor(serviceCatalogRecords),
		datasyncapi.Descriptor(dataSyncRecords),
		accessanalyzerapi.Descriptor(accessAnalyzerRecords),
		shieldapi.Descriptor(shieldRecords),
		globalacceleratorapi.Descriptor(globalAcceleratorRecords),
		fisapi.Descriptor(fisRecords),
		resourceexplorer2api.Descriptor(resourceExplorerRecords),
		securityhubapi.Descriptor(securityHubRecords),
		inspector2api.Descriptor(inspector2Records),
		costexplorerapi.Descriptor(costExplorerRecords),
		costandusagereportserviceapi.Descriptor(curRecords),
		servicequotasapi.Descriptor(quotaRecords),
		databasemigrationserviceapi.Descriptor(dmsRecords),
		emrserverlessapi.Descriptor(emrRecords),
		emrapi.Descriptor(classicEMR),
		opensearchserverlessapi.Descriptor(aossRecords),
		kinesisanalyticsv2api.Descriptor(flinkRecords),
		lakeformationapi.Descriptor(lakeRecords),
		signerapi.Descriptor(signing),
		bedrockapi.Descriptor(foundationModels),
		bedrockruntimeapi.Descriptor(inference),
		mq.ProtectCredentials(mqapi.Descriptor(brokers)),
		kafkaapi.Descriptor(kafkaClusters),
		mwaaapi.Descriptor(airflowEnvironments),
		codedeployapi.Descriptor(codeDeployRecords),
		applicationautoscalingapi.Descriptor(applicationScaling),
		locationapi.Descriptor(locationTrackers),
		s3tablesapi.Descriptor(tableRecords),
		transfer.ProtectPrivateInputs(transferapi.Descriptor(transferRecords)),
		wafapi.Descriptor(wafClassicRecords),
		wafv2api.Descriptor(wafRecords),
		networkfirewallapi.Descriptor(networkFirewallRecords),
		opensearchapi.Descriptor(opensearchRecords),
		route53resolverapi.Descriptor(resolverRecords),
		xrayapi.Descriptor(xrayRecords),
		glacierapi.Descriptor(glacierRecords),
		identitystoreapi.Descriptor(identityStoreRecords),
		ssoadminapi.Descriptor(ssoAdminRecords),
		ramapi.Descriptor(ramRecords),
		acmpcaapi.Descriptor(privateAuthorities),
		verifiedpermissionsapi.Descriptor(verifiedpermissionsRecords),
		redshiftapi.Descriptor(redshiftRecords),
		memorydbapi.Descriptor(memoryDBRecords),
		elbapi.Descriptor(classicBalancers),
		rdsapi.Descriptor(databases),
		rdsdataapi.Descriptor(dataAPI),
		apigatewayv2api.Descriptor(httpAPIs),
		apigatewaymanagementapiapi.Descriptor(connectionAPI),
		apigatewayapi.Descriptor(restAPIs),
		appsyncapi.Descriptor(graphQL),
		sfnapi.Descriptor(workflows),
		kinesisapi.Descriptor(streams),
		route53api.Descriptor(dns),
		cloudfrontapi.Descriptor(cdn),
		schedulerapi.Descriptor(timers),
		autoscalingapi.Descriptor(scaling),
		elbv2api.Descriptor(balancers),
		acmapi.Descriptor(certificates),
		firehoseapi.Descriptor(firehoses),
		pipesapi.Descriptor(pipelines),
		cloudwatchapi.Descriptor(monitoring),
		cloudcontrolapi.Descriptor(control),
		cloudformation.ProtectTemplates(cloudformationapi.Descriptor(stacks)),
		sesapi.Descriptor(ses.New(email)),
		sesv2api.Descriptor(email),
		cognitoidentityapi.Descriptor(identityPools),
		glueapi.Descriptor(dataCatalog),
		athenaapi.Descriptor(queries),
		iotapi.Descriptor(things),
		iotdataplaneapi.Descriptor(things.DataPlane()),
		iotjobsdataplaneapi.Descriptor(things.JobsDataPlane()),
		resourcegroupstaggingapiapi.Descriptor(tags),
		resourcegroupsapi.Descriptor(groups),
	}

	// Step Functions' Task states and Firehose's deliveries call the other
	// services in-process.
	caller := catalog.NewCaller(services...)
	shieldRecords.UseCaller(caller)
	dataSyncRecords.UseCaller(caller)
	wafClassicRecords.UseCaller(caller)
	budgetRecords.Start(caller)
	containers.UseCaller(caller)
	workflows.UseCaller(caller)
	firehoses.UseCaller(caller)
	// Pipes enrich and deliver to Lambda, SQS, SNS, EventBridge, Step Functions, and Kinesis.
	pipelines.UseCaller(caller)
	monitoring.UseCaller(caller)
	dns.UseCaller(caller)
	timers.UseCaller(caller)
	scaling.UseCaller(caller)
	balancers.UseCaller(caller)
	caches.UseCaller(caller)
	databases.UseCaller(caller)
	dataAPI.UseCaller(caller)
	// CloudFormation manages stack resources with the other services'
	// operations.
	control.UseCaller(caller)
	stacks.UseMQ(brokers)
	stacks.UseMSK(kafkaClusters)
	stacks.UseMWAA(airflowEnvironments)
	stacks.UseDataSync(dataSyncRecords)
	stacks.UseTransfer(transferRecords)
	stacks.UseLocation(locationTrackers)
	stacks.UseS3Tables(tableRecords)
	stacks.UseCaller(caller)
	// Subscription filters deliver to Lambda, Kinesis, and Firehose.
	logGroups.UseCaller(caller)
	// Table exports write to S3.
	tables.UseCaller(caller)
	// Asynchronous invocations send results to their destinations.
	functions.UseCaller(caller)
	// GraphQL data sources reach DynamoDB and EventBridge.
	graphQL.UseCaller(caller)
	email.UseCaller(caller)
	identityPools.UseCaller(caller)
	// Topic rules' actions reach the other services.
	things.UseCaller(caller)
	// The Tagging API reads every service's resources and tags them through
	// their own operations.
	tags.UseCaller(caller)
	// Groups resolve their queries against the same resources, and read CloudFormation stacks.
	groups.UseCaller(caller)
	resourceExplorerRecords.UseCaller(caller)
	fisRecords.UseCaller(caller)
	// The credentials provider exchanges certificates for role credentials.
	things.UseRoleCredentials(tokens.AssumeRoleForService)
	handler, err := gateway.New(cfg, logger, services...)
	if err != nil {
		return nil, err
	}

	handler.UseManagementEvents(trails.Record)
	handler.UseCredentials(tokens.Credentials)
	configureShell(handler, tokens)
	handler.UseAuthorization(roles.Authorization)
	// Deliveries from one service to another need the target's resource policy to allow the service.
	handler.UseDeliveryTargets(map[string]gateway.DeliveryTarget{
		"sqs": queues.DeliveryResource, "sns": topics.DeliveryResource, "lambda": functions.DeliveryResource,
	})

	// Deliveries a service makes under a role need the role's trust policy to allow the service, and the role to
	// be permitted the call.
	handler.UseServiceRoles(tokens.ServiceSession)
	functions.UseServiceAuthorizer(handler.AuthorizeService)
	// Pollers read event sources as the function's execution role, so they start once the authorizer is set.
	if err = functions.Resume(ctx); err != nil {
		return nil, err
	}

	topics.UseServiceAuthorizer(handler.AuthorizeService)
	balancers.UseServiceAuthorizer(handler.AuthorizeService)
	logGroups.UseServiceAuthorizer(handler.AuthorizeService)
	monitoring.UseServiceAuthorizer(handler.AuthorizeService)
	buckets.UseServiceAuthorizer(handler.AuthorizeService)
	rules.UseServiceAuthorizer(handler.AuthorizeService)
	restAPIs.UseServiceAuthorizer(handler.AuthorizeService)
	httpAPIs.UseServiceAuthorizer(handler.AuthorizeService)
	timers.UseServiceAuthorizer(handler.AuthorizeService)
	pipelines.UseServiceAuthorizer(handler.AuthorizeService)
	workflows.UseServiceAuthorizer(handler.AuthorizeService)
	things.UseServiceAuthorizer(handler.AuthorizeService)
	firehoses.UseServiceAuthorizer(handler.AuthorizeService)
	handler.UseInstanceProfiles(roles.InstanceProfileOf)
	if cfg.Identity.Mode == config.IdentityVerify {
		// Web identity tokens must carry a valid signature of their issuer, or of
		// the identity pool that issued them.
		tokens.UseWebIdentityVerification(proxy.NewVerifier().Verify, identityPools.VerifyOpenIDToken)
	}

	if oidc := cfg.Console.Auth.OIDC; oidc != nil {
		login, ssoErr := sso.New(oidc, tokens, logger)
		if ssoErr != nil {
			return nil, ssoErr
		}

		if cfg.Identity.Authorization == config.AuthorizationOff || cfg.Identity.Authorization == "" {
			logger.Warn("console single sign-on identifies people but does not limit them: " +
				"set identity.authorization to soft or enforce")
		}

		for name, h := range login.Handlers() {
			handler.HandleConsole(name, h)
		}
	}

	handler.UseDocuments(identities.Documents)
	handler.UseTokens(identities.Token)
	handler.UseHosted(identities)
	handler.UseRegistry(registry)
	handler.UseCORS(buckets.CORS)
	handler.UseAPIs(connectionAPI, httpAPIs, restAPIs)
	handler.UseDomains(restAPIs.Domains())
	handler.UseLoadBalancers(balancers)
	handler.UseFunctionURLs(functions.URLs())
	handler.UseGraphQL(graphQL)
	handler.UseMQTT(things)
	if cfg.EKS.Clusters == config.ClustersK3s {
		// Cluster endpoints accept the EKS tokens the gateway's credentials
		// sign.
		clusters, k3sErr := k3s.New(handler.KubernetesAccount, logger)
		if k3sErr != nil {
			return nil, k3sErr
		}

		kube.UseRuntime(clusters, logger)
	}

	if cfg.RDS.Instances == config.InstancesDocker {
		if err = databases.UseRuntime(rdsdocker.New()); err != nil {
			return nil, err
		}
	}

	if cfg.ElastiCache.Caches == config.CachesDocker {
		caches.UseRuntime(cachedocker.New(), logger)
	}

	bedrockFixtures := bedrockruntime.Handler(store, cfg.Defaults.Account)
	handler.Handle("bedrock/fixtures", bedrockFixtures)
	handler.Handle("bedrock/fixtures/", bedrockFixtures)
	fixtures := costdata.Handler(store, cfg.Defaults.Account)
	handler.Handle("costs/fixtures", fixtures)
	handler.Handle("costs/fixtures/", fixtures)
	handler.Handle("cloudformation/responses/", gateway.CloudFormationResponses(stacks.RespondCustomResource))
	handler.Handle("deliveries", deliveries)
	handler.Handle("messages", delivery.MessagesHandler(deliveries))
	handler.HandlePath("/_aws/ses", delivery.SESMessagesHandler(deliveries))
	handler.HandlePath("/_aws/sns/sms-messages", delivery.SMSMessagesHandler(deliveries))
	handler.Handle("iot/root-ca.pem", things.RootCA())
	handler.Handle("reset", http.HandlerFunc(a.serveReset))

	metaService := metadata.New(
		network, containers,
		metadata.WithAssumeRole(tokens.AssumeRoleForService),
		metadata.WithInstanceProfiles(roles.InstanceProfileOf),
		metadata.WithDefaultInstanceID(cfg.Metadata.IMDS.DefaultInstanceID),
		metadata.WithVerifyIdentity(cfg.Identity.Mode == config.IdentityVerify),
	)
	a.Metadata = metaService
	handler.HandlePath("/_tinystack/imds/", metaService.IMDSHandler())
	handler.HandlePath("/_tinystack/ecs/", metaService.ECSHandler())
	// LocalStack's path answers the same, for tools that wait on it. Scripts
	// must end before the services they call stop, so this stop is registered last.
	for _, prefix := range []string{"/_tinystack/init", "/_localstack/init"} {
		for _, path := range inithooks.Paths(prefix) {
			handler.HandlePath(path, hooks)
		}
	}

	if cfg.DNS.Enabled {
		listener, dnsErr := dnsserver.Start(ctx, cfg.DNS.Address, cfg.Server.Host, cfg.DNS.Upstreams,
			func(ctx context.Context, name string, kind uint16) ([]wiredns.RR, bool, error) {
				return dns.LookupDNS(ctx, cfg.Defaults.Account, name, kind)
			})
		if dnsErr != nil {
			return nil, dnsErr
		}

		a.onStop(listener.Stop)
	}

	a.onStop(hooks.Stop)
	a.Gateway = handler
	a.onReset(func(context.Context) { handler.ResetFaults() })
	a.IoT = things
	for _, service := range services {
		a.Services = append(a.Services, service.Name)
	}

	return a, nil
}

// Stop ends every service's background work and runtimes, the most recently
// built first, so nothing writes to the store afterwards. It is safe to call
// more than once and alongside StopOnShutdown.
func (a *App) Stop() {
	a.once.Do(func() {
		for _, stop := range slices.Backward(a.stops) {
			stop()
		}
	})
}

// StopOnShutdown has server start every service's Stop, each in its own
// goroutine, when it begins shutting down, so long polls and streams end and
// shutdown need not wait out one slow runtime. Stop still waits for them all.
func (a *App) StopOnShutdown(server *http.Server) {
	for _, stop := range a.stops {
		server.RegisterOnShutdown(stop)
	}
}

// onReset has Reset call reset, before it clears the store, so the service
// can read its records.
func (a *App) onReset(reset func(context.Context)) {
	a.resets = append(a.resets, reset)
}

func (a *App) onStop(stop func()) {
	a.stops = append(a.stops, stop)
}
